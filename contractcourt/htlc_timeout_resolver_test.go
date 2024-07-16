package contractcourt

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	dummyBytes    = []byte{0}
	preimageBytes = bytes.Repeat([]byte{1}, lntypes.HashSize)
)

type mockWitnessBeacon struct {
	preImageUpdates chan lntypes.Preimage
	newPreimages    chan []lntypes.Preimage
	lookupPreimage  map[lntypes.Hash]lntypes.Preimage
}

func newMockWitnessBeacon() *mockWitnessBeacon {
	return &mockWitnessBeacon{
		preImageUpdates: make(chan lntypes.Preimage, 1),
		newPreimages:    make(chan []lntypes.Preimage, 1),
		lookupPreimage:  make(map[lntypes.Hash]lntypes.Preimage),
	}
}

func (m *mockWitnessBeacon) SubscribeUpdates(
	chanID lnwire.ShortChannelID, htlc *channeldb.HTLC,
	payload *hop.Payload,
	nextHopOnionBlob []byte) (*WitnessSubscription, error) {

	return &WitnessSubscription{
		WitnessUpdates:     m.preImageUpdates,
		CancelSubscription: func() {},
	}, nil
}

func (m *mockWitnessBeacon) LookupPreimage(payhash lntypes.Hash) (lntypes.Preimage, bool) {
	preimage, ok := m.lookupPreimage[payhash]
	if !ok {
		return lntypes.Preimage{}, false
	}
	return preimage, true
}

func (m *mockWitnessBeacon) AddPreimages(preimages ...lntypes.Preimage) error {
	m.newPreimages <- preimages
	return nil
}

// NOTE: the following tests essentially checks many of the same scenarios as
// the test above, but they expand on it by checking resuming from checkpoints
// at every stage.

// TestHtlcTimeoutSingleStage tests a remote commitment confirming, and the
// local node sweeping the HTLC output directly after timeout.
//
//nolint:lll
func TestHtlcTimeoutSingleStage(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 3}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}

	// singleStageResolution is a resolution for a htlc on the remote
	// party's commitment.
	singleStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint: commitOutpoint,
		SweepSignDesc: testSignDesc,
	}

	sweepTxid := sweepTx.TxHash()
	claim := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &sweepTxid,
	}

	sweepSpend := &chainntnfs.SpendDetail{
		SpendingTx:    sweepTx,
		SpentOutPoint: &commitOutpoint,
		SpenderTxHash: &sweepTxid,
	}

	checkpoints := []checkpoint{
		{
			// Output should be handed off to the nursery.
			incubating: true,
		},
		{
			// We send a confirmation the sweep tx from published
			// by the nursery.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {
				// The nursery will create and publish a sweep
				// tx.
				select {
				case ctx.notifier.SpendChan <- sweepSpend:
				case <-time.After(time.Second * 5):
					t.Fatalf("failed to send spend ntfn")
				}

				// The resolver should deliver a failure
				// resolition message (indicating we
				// successfully timed out the HTLC).
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if resolutionMsg.Failure == nil {
						t.Fatalf("expected failure resolution msg")
					}
				case <-time.After(time.Second * 5):
					t.Fatalf("resolution not sent")
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// report.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
		},
	}

	testHtlcTimeout(
		t, singleStageResolution, checkpoints,
	)
}

// TestHtlcTimeoutSecondStage tests a local commitment being confirmed, and the
// local node claiming the HTLC output using the second-level timeout tx.
//
//nolint:lll
func TestHtlcTimeoutSecondStage(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	htlcOutpoint := wire.OutPoint{Index: 3}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}
	sweepHash := sweepTx.TxHash()

	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    111,
				PkScript: []byte{0xaa, 0xaa},
			},
		},
	}

	signer := &mock.DummySigner{}
	witness, err := input.SenderHtlcSpendTimeout(
		&mock.DummySignature{}, txscript.SigHashAll,
		signer, &testSignDesc, timeoutTx,
	)
	require.NoError(t, err)
	timeoutTx.TxIn[0].Witness = witness

	timeoutTxid := timeoutTx.TxHash()

	// twoStageResolution is a resolution for a htlc on the local
	// party's commitment.
	twoStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint:   htlcOutpoint,
		SignedTimeoutTx: timeoutTx,
		SweepSignDesc:   testSignDesc,
	}

	firstStage := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &timeoutTxid,
	}

	secondState := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &sweepHash,
	}

	timeoutSpend := &chainntnfs.SpendDetail{
		SpendingTx:    timeoutTx,
		SpentOutPoint: &commitOutpoint,
		SpenderTxHash: &timeoutTxid,
	}

	sweepSpend := &chainntnfs.SpendDetail{
		SpendingTx:    sweepTx,
		SpentOutPoint: &htlcOutpoint,
		SpenderTxHash: &sweepHash,
	}

	checkpoints := []checkpoint{
		{
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				// Deliver spend of timeout tx.
				ctx.notifier.SpendChan <- timeoutSpend
				ctx.notifier.SpendChan <- timeoutSpend

				return nil
			},

			// Output should be handed off to the nursery.
			incubating: true,
			reports: []*channeldb.ResolverReport{
				firstStage,
			},
		},
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				resumed bool) error {

				// When it's reloaded from disk, we need to
				// re-send the notification to mock the first
				// `watchHtlcSpend`.
				if resumed {
					// Deliver spend of timeout tx output.
					ctx.notifier.SpendChan <- timeoutSpend

					// Deliver spend of timeout tx output.
					ctx.notifier.SpendChan <- sweepSpend
					ctx.notifier.SpendChan <- sweepSpend

					return nil
				}

				// Deliver spend of timeout tx output.
				ctx.notifier.SpendChan <- sweepSpend

				// The resolver should deliver a failure
				// resolution message (indicating we
				// successfully timed out the HTLC).
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if resolutionMsg.Failure == nil {
						t.Fatalf("expected failure resolution msg")
					}
				case <-time.After(time.Second * 1):
					t.Fatalf("resolution not sent")
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// reports.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				secondState,
			},
		},
	}

	testHtlcTimeout(
		t, twoStageResolution, checkpoints,
	)
}

// TestHtlcTimeoutSingleStageRemoteSpend tests that when a local commitment
// confirms, and the remote spends the HTLC output directly, we detect this and
// extract the preimage.
func TestHtlcTimeoutSingleStageRemoteSpend(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	htlcOutpoint := wire.OutPoint{Index: 3}

	spendTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}

	fakePreimageBytes := bytes.Repeat([]byte{1}, lntypes.HashSize)
	var fakePreimage lntypes.Preimage
	copy(fakePreimage[:], fakePreimageBytes)

	signer := &mock.DummySigner{}
	witness, err := input.SenderHtlcSpendRedeem(
		signer, &testSignDesc, spendTx,
		fakePreimageBytes,
	)
	require.NoError(t, err)
	spendTx.TxIn[0].Witness = witness

	spendTxHash := spendTx.TxHash()

	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    123,
				PkScript: []byte{0xff, 0xff},
			},
		},
	}

	timeoutWitness, err := input.SenderHtlcSpendTimeout(
		&mock.DummySignature{}, txscript.SigHashAll,
		signer, &testSignDesc, timeoutTx,
	)
	require.NoError(t, err)
	timeoutTx.TxIn[0].Witness = timeoutWitness

	// twoStageResolution is a resolution for a htlc on the local
	// party's commitment.
	twoStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint:   htlcOutpoint,
		SignedTimeoutTx: timeoutTx,
		SweepSignDesc:   testSignDesc,
	}

	claim := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendTxHash,
	}

	checkpoints := []checkpoint{
		{
			// We send a spend notification for a remote spend with
			// the preimage.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				witnessBeacon := ctx.resolver.(*htlcTimeoutResolver).PreimageDB.(*mockWitnessBeacon)

				// The remote spends the output directly with
				// the preimage.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    spendTx,
					SpentOutPoint: &commitOutpoint,
					SpenderTxHash: &spendTxHash,
				}

				// We should extract the preimage.
				select {
				case newPreimage := <-witnessBeacon.newPreimages:
					if newPreimage[0] != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, newPreimage)
					}

				case <-time.After(time.Second * 5):
					t.Fatalf("pre-image not added")
				}

				// Finally, we should get a resolution message
				// with the pre-image set within the message.
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if *resolutionMsg.PreImage != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, resolutionMsg.PreImage)
					}
				case <-time.After(time.Second * 5):
					t.Fatalf("resolution not sent")
				}

				return nil
			},

			// After the success tx has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// report.
			incubating: false,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
		},
	}

	testHtlcTimeout(
		t, twoStageResolution, checkpoints,
	)
}

// TestHtlcTimeoutSecondStageRemoteSpend tests that when a remite commitment
// confirms, and the remote spends the output using the success tx, we
// properly detect this and extract the preimage.
func TestHtlcTimeoutSecondStageRemoteSpend(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}

	remoteSuccessTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{},
	}

	fakePreimageBytes := bytes.Repeat([]byte{1}, lntypes.HashSize)
	var fakePreimage lntypes.Preimage
	copy(fakePreimage[:], fakePreimageBytes)

	signer := &mock.DummySigner{}
	witness, err := input.ReceiverHtlcSpendRedeem(
		&mock.DummySignature{}, txscript.SigHashAll,
		fakePreimageBytes, signer,
		&testSignDesc, remoteSuccessTx,
	)
	require.NoError(t, err)

	remoteSuccessTx.TxIn[0].Witness = witness
	successTxid := remoteSuccessTx.TxHash()

	// singleStageResolution allwoing the local node to sweep HTLC output
	// directly from the remote commitment after timeout.
	singleStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint: commitOutpoint,
		SweepSignDesc: testSignDesc,
	}

	claim := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &successTxid,
	}

	checkpoints := []checkpoint{
		{
			// Output should be handed off to the nursery.
			incubating: true,
		},
		{
			// We send a confirmation for the remote's second layer
			// success transcation.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    remoteSuccessTx,
					SpentOutPoint: &commitOutpoint,
					SpenderTxHash: &successTxid,
				}

				witnessBeacon := ctx.resolver.(*htlcTimeoutResolver).PreimageDB.(*mockWitnessBeacon)

				// We expect the preimage to be extracted,
				select {
				case newPreimage := <-witnessBeacon.newPreimages:
					if newPreimage[0] != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, newPreimage)
					}

				case <-time.After(time.Second * 5):
					t.Fatalf("pre-image not added")
				}

				// Finally, we should get a resolution message with the
				// pre-image set within the message.
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if *resolutionMsg.PreImage != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, resolutionMsg.PreImage)
					}
				case <-time.After(time.Second * 5):
					t.Fatalf("resolution not sent")
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// report.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
		},
	}

	testHtlcTimeout(
		t, singleStageResolution, checkpoints,
	)
}

// TestHtlcTimeoutSecondStageSweeper tests that for anchor channels, when a
// local commitment confirms, the timeout tx is handed to the sweeper to claim
// the HTLC output.
//
//nolint:lll
func TestHtlcTimeoutSecondStageSweeper(t *testing.T) {
	htlcOutpoint := wire.OutPoint{Index: 3}

	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    123,
				PkScript: []byte{0xff, 0xff},
			},
		},
	}

	// We set the timeout witness since the script is used when subscribing
	// to spends.
	signer := &mock.DummySigner{}
	timeoutWitness, err := input.SenderHtlcSpendTimeout(
		&mock.DummySignature{}, txscript.SigHashAll,
		signer, &testSignDesc, timeoutTx,
	)
	require.NoError(t, err)
	timeoutTx.TxIn[0].Witness = timeoutWitness

	reSignedTimeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{0xaa, 0xbb},
					Index: 0,
				},
			},
			timeoutTx.TxIn[0],
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{0xaa, 0xbb},
					Index: 2,
				},
			},
		},

		TxOut: []*wire.TxOut{
			{
				Value:    111,
				PkScript: []byte{0xaa, 0xaa},
			},
			timeoutTx.TxOut[0],
		},
	}
	reSignedHash := reSignedTimeoutTx.TxHash()

	timeoutTxOutpoint := wire.OutPoint{
		Hash:  reSignedHash,
		Index: 1,
	}

	// Make a copy so `isPreimageSpend` can easily pass.
	sweepTx := reSignedTimeoutTx.Copy()
	sweepHash := sweepTx.TxHash()

	// twoStageResolution is a resolution for a htlc on the local
	// party's commitment, where the timeout tx can be re-signed.
	twoStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint:   htlcOutpoint,
		SignedTimeoutTx: timeoutTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		SweepSignDesc: testSignDesc,
	}

	firstStage := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &reSignedHash,
	}

	secondState := &channeldb.ResolverReport{
		OutPoint:        timeoutTxOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &sweepHash,
	}
	// mockTimeoutTxSpend is a helper closure to mock `waitForSpend` to
	// return the commit spend in `sweepTimeoutTxOutput`.
	mockTimeoutTxSpend := func(ctx *htlcResolverTestContext) {
		select {
		case ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
			SpendingTx:        reSignedTimeoutTx,
			SpenderInputIndex: 1,
			SpenderTxHash:     &reSignedHash,
			SpendingHeight:    10,
			SpentOutPoint:     &htlcOutpoint,
		}:

		case <-time.After(time.Second * 1):
			t.Fatalf("spend not sent")
		}
	}

	// mockSweepTxSpend is a helper closure to mock `waitForSpend` to
	// return the commit spend in `sweepTimeoutTxOutput`.
	mockSweepTxSpend := func(ctx *htlcResolverTestContext) {
		select {
		case ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
			SpendingTx:        sweepTx,
			SpenderInputIndex: 1,
			SpenderTxHash:     &sweepHash,
			SpendingHeight:    10,
			SpentOutPoint:     &timeoutTxOutpoint,
		}:

		case <-time.After(time.Second * 1):
			t.Fatalf("spend not sent")
		}
	}

	checkpoints := []checkpoint{
		{
			// The output should be given to the sweeper.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				resolver := ctx.resolver.(*htlcTimeoutResolver)

				var (
					inp input.Input
					ok  bool
				)

				select {
				case inp, ok = <-resolver.Sweeper.(*mockSweeper).sweptInputs:
					require.True(t, ok)

				case <-time.After(1 * time.Second):
					t.Fatal("expected input to be swept")
				}

				op := inp.OutPoint()
				if op != htlcOutpoint {
					return fmt.Errorf("outpoint %v swept, "+
						"expected %v", op, htlcOutpoint)
				}

				// Mock `waitForSpend` twice, called in,
				// - `resolveReSignedTimeoutTx`
				// - `sweepTimeoutTxOutput`.
				mockTimeoutTxSpend(ctx)
				mockTimeoutTxSpend(ctx)

				return nil
			},
			// incubating=true is used to signal that the
			// second-level transaction was confirmed.
			incubating: true,
			reports: []*channeldb.ResolverReport{
				firstStage,
			},
		},
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				resumed bool) error {

				// Mock `waitForSpend` to return the commit
				// spend.
				if resumed {
					mockTimeoutTxSpend(ctx)
					mockTimeoutTxSpend(ctx)
					mockSweepTxSpend(ctx)

					return nil
				}

				mockSweepTxSpend(ctx)

				// The resolver should deliver a failure
				// resolution message (indicating we
				// successfully timed out the HTLC).
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if resolutionMsg.Failure == nil {
						t.Fatalf("expected failure resolution msg")
					}
				case <-time.After(time.Second * 1):
					t.Fatalf("resolution not sent")
				}

				// The timeout tx output should now be given to
				// the sweeper.
				resolver := ctx.resolver.(*htlcTimeoutResolver)

				var (
					inp input.Input
					ok  bool
				)

				select {
				case inp, ok = <-resolver.Sweeper.(*mockSweeper).sweptInputs:
					require.True(t, ok)

				case <-time.After(1 * time.Second):
					t.Fatal("expected input to be swept")
				}

				op := inp.OutPoint()
				exp := wire.OutPoint{
					Hash:  reSignedHash,
					Index: 1,
				}
				if op != exp {
					return fmt.Errorf("wrong outpoint swept")
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// reports.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				secondState,
			},
		},
	}

	testHtlcTimeout(
		t, twoStageResolution, checkpoints,
	)
}

// TestHtlcTimeoutSecondStageSweeperRemoteSpend tests that if a local timeout
// tx is offered to the sweeper, but the output is swept by the remote node, we
// properly detect this and extract the preimage.
func TestHtlcTimeoutSecondStageSweeperRemoteSpend(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	htlcOutpoint := wire.OutPoint{Index: 3}

	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    123,
				PkScript: []byte{0xff, 0xff},
			},
		},
	}

	// We set the timeout witness since the script is used when subscribing
	// to spends.
	signer := &mock.DummySigner{}
	timeoutWitness, err := input.SenderHtlcSpendTimeout(
		&mock.DummySignature{}, txscript.SigHashAll,
		signer, &testSignDesc, timeoutTx,
	)
	require.NoError(t, err)
	timeoutTx.TxIn[0].Witness = timeoutWitness

	spendTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}

	fakePreimageBytes := bytes.Repeat([]byte{1}, lntypes.HashSize)
	var fakePreimage lntypes.Preimage
	copy(fakePreimage[:], fakePreimageBytes)

	witness, err := input.SenderHtlcSpendRedeem(
		signer, &testSignDesc, spendTx,
		fakePreimageBytes,
	)
	require.NoError(t, err)
	spendTx.TxIn[0].Witness = witness

	spendTxHash := spendTx.TxHash()

	// twoStageResolution is a resolution for a htlc on the local
	// party's commitment, where the timeout tx can be re-signed.
	twoStageResolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint:   htlcOutpoint,
		SignedTimeoutTx: timeoutTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		SweepSignDesc: testSignDesc,
	}

	claim := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendTxHash,
	}

	checkpoints := []checkpoint{
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				resumed bool) error {

				// If we are resuming from a checkpoint, we
				// expect the resolver to re-subscribe to a
				// spend, hence we must resend it.
				if resumed {
					t.Logf("resumed")
					ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
						SpendingTx:    spendTx,
						SpenderTxHash: &spendTxHash,
						SpentOutPoint: &commitOutpoint,
					}
				}

				witnessBeacon := ctx.resolver.(*htlcTimeoutResolver).PreimageDB.(*mockWitnessBeacon)

				// We should extract the preimage.
				select {
				case newPreimage := <-witnessBeacon.newPreimages:
					if newPreimage[0] != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, newPreimage)
					}

				case <-time.After(time.Second * 5):
					t.Fatalf("pre-image not added")
				}

				// Finally, we should get a resolution message
				// with the pre-image set within the message.
				select {
				case resolutionMsg := <-ctx.resolutionChan:
					if *resolutionMsg.PreImage != fakePreimage {
						t.Fatalf("wrong pre-image: "+
							"expected %v, got %v",
							fakePreimage, resolutionMsg.PreImage)
					}
				case <-time.After(time.Second * 5):
					t.Fatalf("resolution not sent")
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// reports.
			incubating: false,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
		},
	}

	testHtlcTimeout(
		t, twoStageResolution, checkpoints,
	)
}

func testHtlcTimeout(t *testing.T, resolution lnwallet.OutgoingHtlcResolution,
	checkpoints []checkpoint) {

	defer timeout()()

	// We first run the resolver from start to finish, ensuring it gets
	// checkpointed at every expected stage. We store the checkpointed data
	// for the next portion of the test.
	ctx := newHtlcResolverTestContext(t,
		func(htlc channeldb.HTLC, cfg ResolverConfig) ContractResolver {
			r := &htlcTimeoutResolver{
				contractResolverKit: *newContractResolverKit(cfg),
				htlc:                htlc,
				htlcResolution:      resolution,
			}
			r.initLogger("htlcTimeoutResolver")

			return r
		},
	)

	checkpointedState := runFromCheckpoint(t, ctx, checkpoints)

	t.Log("Running resolver to completion after restart")

	// Now, from every checkpoint created, we re-create the resolver, and
	// run the test from that checkpoint.
	for i := range checkpointedState {
		cp := bytes.NewReader(checkpointedState[i])
		ctx := newHtlcResolverTestContextFromReader(t,
			func(htlc channeldb.HTLC, cfg ResolverConfig) ContractResolver {
				resolver, err := newTimeoutResolverFromReader(cp, cfg)
				if err != nil {
					t.Fatal(err)
				}

				resolver.Supplement(htlc)
				resolver.initLogger("htlcTimeoutResolver")

				return resolver
			},
		)

		// Run from the given checkpoint, ensuring we'll hit the rest.
		_ = runFromCheckpoint(t, ctx, checkpoints[i+1:])
	}
}

// TestCheckSizeAndIndex checks that the `checkSizeAndIndex` behaves as
// expected.
func TestCheckSizeAndIndex(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		witness  wire.TxWitness
		size     int
		index    int
		expected bool
	}{
		{
			// Test that a witness with the correct size and index
			// for the preimage.
			name: "valid preimage",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes,
			},
			size:     2,
			index:    1,
			expected: true,
		},
		{
			// Test that a witness with the wrong size.
			name: "wrong witness size",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes,
			},
			size:     3,
			index:    1,
			expected: false,
		},
		{
			// Test that a witness with the right size but wrong
			// preimage index.
			name: "wrong preimage index",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes,
			},
			size:     2,
			index:    0,
			expected: false,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := checkSizeAndIndex(
				tc.witness, tc.size, tc.index,
			)
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestIsPreimageSpend tests `isPreimageSpend` can successfully detect a
// preimage spend based on whether the commitment is local or remote.
func TestIsPreimageSpend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		witness     wire.TxWitness
		isTaproot   bool
		localCommit bool
	}{
		{
			// Test a preimage spend on the remote commitment for
			// taproot channels.
			name: "tap preimage spend on remote",
			witness: wire.TxWitness{
				dummyBytes, dummyBytes, preimageBytes,
				dummyBytes, dummyBytes,
			},
			isTaproot:   true,
			localCommit: false,
		},
		{
			// Test a preimage spend on the local commitment for
			// taproot channels.
			name: "tap preimage spend on local",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes,
				dummyBytes, dummyBytes,
			},
			isTaproot:   true,
			localCommit: true,
		},
		{
			// Test a preimage spend on the remote commitment for
			// non-taproot channels.
			name: "preimage spend on remote",
			witness: wire.TxWitness{
				dummyBytes, dummyBytes, dummyBytes,
				preimageBytes, dummyBytes,
			},
			isTaproot:   false,
			localCommit: false,
		},
		{
			// Test a preimage spend on the local commitment for
			// non-taproot channels.
			name: "preimage spend on local",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes, dummyBytes,
			},
			isTaproot:   false,
			localCommit: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		// Run the test.
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a test spend detail that spends the HTLC
			// output.
			spend := &chainntnfs.SpendDetail{
				SpendingTx:        &wire.MsgTx{},
				SpenderInputIndex: 0,
			}

			// Attach the testing witness.
			spend.SpendingTx.TxIn = []*wire.TxIn{{
				Witness: tc.witness,
			}}

			result := isPreimageSpend(
				tc.isTaproot, spend, tc.localCommit,
			)
			require.True(t, result)
		})
	}
}
