package contractcourt

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
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
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type mockWitnessBeacon struct {
	preImageUpdates chan lntypes.Preimage
	newPreimages    chan []lntypes.Preimage
	lookupPreimage  map[lntypes.Hash]lntypes.Preimage
}

func newMockWitnessBeacon() *mockWitnessBeacon {
	return &mockWitnessBeacon{
		preImageUpdates: make(chan lntypes.Preimage, 1),
		newPreimages:    make(chan []lntypes.Preimage),
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

// TestHtlcTimeoutResolver tests that the timeout resolver properly handles all
// variations of possible local+remote spends.
func TestHtlcTimeoutResolver(t *testing.T) {
	t.Parallel()

	fakePreimageBytes := bytes.Repeat([]byte{1}, lntypes.HashSize)

	var (
		htlcOutpoint wire.OutPoint
		fakePreimage lntypes.Preimage
	)
	fakeSignDesc := &input.SignDescriptor{
		Output: &wire.TxOut{},
	}

	copy(fakePreimage[:], fakePreimageBytes)

	signer := &mock.DummySigner{}
	sweepTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
				Witness:          [][]byte{{0x01}},
			},
		},
	}
	fakeTimeout := int32(5)

	templateTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
	}

	testCases := []struct {
		// name is a human readable description of the test case.
		name string

		// remoteCommit denotes if the commitment broadcast was the
		// remote commitment or not.
		remoteCommit bool

		// timeout denotes if the HTLC should be let timeout, or if the
		// "remote" party should sweep it on-chain. This also affects
		// what type of resolution message we expect.
		timeout bool

		// txToBroadcast is a function closure that should generate the
		// transaction that should spend the HTLC output. Test authors
		// can use this to customize the witness used when spending to
		// trigger various redemption cases.
		txToBroadcast func() (*wire.MsgTx, error)

		// outcome is the resolver outcome that we expect to be reported
		// once the contract is fully resolved.
		outcome channeldb.ResolverOutcome
	}{
		// Remote commitment is broadcast, we time out the HTLC on
		// chain, and should expect a fail HTLC resolution.
		{
			name:         "timeout remote tx",
			remoteCommit: true,
			timeout:      true,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.ReceiverHtlcSpendTimeout(
					signer, fakeSignDesc, sweepTx,
					fakeTimeout,
				)
				if err != nil {
					return nil, err
				}

				// To avoid triggering the race detector by
				// setting the witness the second time this
				// method is called during tests, we return
				// immediately if the witness is already set
				// correctly.
				if reflect.DeepEqual(
					templateTx.TxIn[0].Witness, witness,
				) {

					return templateTx, nil
				}
				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
			outcome: channeldb.ResolverOutcomeTimeout,
		},

		// Our local commitment is broadcast, we timeout the HTLC and
		// still expect an HTLC fail resolution.
		{
			name:         "timeout local tx",
			remoteCommit: false,
			timeout:      true,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.SenderHtlcSpendTimeout(
					&mock.DummySignature{}, txscript.SigHashAll,
					signer, fakeSignDesc, sweepTx,
				)
				if err != nil {
					return nil, err
				}

				// To avoid triggering the race detector by
				// setting the witness the second time this
				// method is called during tests, we return
				// immediately if the witness is already set
				// correctly.
				if reflect.DeepEqual(
					templateTx.TxIn[0].Witness, witness,
				) {

					return templateTx, nil
				}

				templateTx.TxIn[0].Witness = witness

				// Set the outpoint to be on our commitment, since
				// we need to claim in two stages.
				templateTx.TxIn[0].PreviousOutPoint = testChanPoint1
				return templateTx, nil
			},
			outcome: channeldb.ResolverOutcomeTimeout,
		},

		// The remote commitment is broadcast, they sweep with the
		// pre-image, we should get a settle HTLC resolution.
		{
			name:         "success remote tx",
			remoteCommit: true,
			timeout:      false,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.ReceiverHtlcSpendRedeem(
					&mock.DummySignature{}, txscript.SigHashAll,
					fakePreimageBytes, signer, fakeSignDesc,
					sweepTx,
				)
				if err != nil {
					return nil, err
				}

				// To avoid triggering the race detector by
				// setting the witness the second time this
				// method is called during tests, we return
				// immediately if the witness is already set
				// correctly.
				if reflect.DeepEqual(
					templateTx.TxIn[0].Witness, witness,
				) {

					return templateTx, nil
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
			outcome: channeldb.ResolverOutcomeClaimed,
		},

		// The local commitment is broadcast, they sweep it with a
		// timeout from the output, and we should still get the HTLC
		// settle resolution back.
		{
			name:         "success local tx",
			remoteCommit: false,
			timeout:      false,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.SenderHtlcSpendRedeem(
					signer, fakeSignDesc, sweepTx,
					fakePreimageBytes,
				)
				if err != nil {
					return nil, err
				}

				// To avoid triggering the race detector by
				// setting the witness the second time this
				// method is called during tests, we return
				// immediately if the witness is already set
				// correctly.
				if reflect.DeepEqual(
					templateTx.TxIn[0].Witness, witness,
				) {

					return templateTx, nil
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
			outcome: channeldb.ResolverOutcomeClaimed,
		},
	}

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	witnessBeacon := newMockWitnessBeacon()

	for _, testCase := range testCases {
		t.Logf("Running test case: %v", testCase.name)

		checkPointChan := make(chan struct{}, 1)
		incubateChan := make(chan struct{}, 1)
		resolutionChan := make(chan ResolutionMsg, 1)
		reportChan := make(chan *channeldb.ResolverReport)

		chainCfg := ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				Notifier:   notifier,
				PreimageDB: witnessBeacon,
				IncubateOutputs: func(wire.OutPoint,
					*lnwallet.OutgoingHtlcResolution,
					*lnwallet.IncomingHtlcResolution,
					uint32) error {

					incubateChan <- struct{}{}
					return nil
				},
				DeliverResolutionMsg: func(msgs ...ResolutionMsg) error {
					if len(msgs) != 1 {
						return fmt.Errorf("expected 1 "+
							"resolution msg, instead got %v",
							len(msgs))
					}

					resolutionChan <- msgs[0]
					return nil
				},
			},
			PutResolverReport: func(_ kvdb.RwTx,
				_ *channeldb.ResolverReport) error {

				return nil
			},
		}

		cfg := ResolverConfig{
			ChannelArbitratorConfig: chainCfg,
			Checkpoint: func(_ ContractResolver,
				reports ...*channeldb.ResolverReport) error {

				checkPointChan <- struct{}{}

				// Send all of our reports into the channel.
				for _, report := range reports {
					reportChan <- report
				}

				return nil
			},
		}
		resolver := &htlcTimeoutResolver{
			htlcResolution: lnwallet.OutgoingHtlcResolution{
				ClaimOutpoint: testChanPoint2,
				SweepSignDesc: *fakeSignDesc,
			},
			contractResolverKit: *newContractResolverKit(
				cfg,
			),
			htlc: channeldb.HTLC{
				Amt: testHtlcAmt,
			},
		}

		var reports []*channeldb.ResolverReport

		// If the test case needs the remote commitment to be
		// broadcast, then we'll set the timeout commit to a fake
		// transaction to force the code path.
		if !testCase.remoteCommit {
			timeoutTx, err := testCase.txToBroadcast()
			require.NoError(t, err)

			resolver.htlcResolution.SignedTimeoutTx = timeoutTx

			if testCase.timeout {
				timeoutTxID := timeoutTx.TxHash()
				reports = append(reports, &channeldb.ResolverReport{
					OutPoint:        timeoutTx.TxIn[0].PreviousOutPoint,
					Amount:          testHtlcAmt.ToSatoshis(),
					ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
					ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
					SpendTxID:       &timeoutTxID,
				})
			}
		}

		// With all the setup above complete, we can initiate the
		// resolution process, and the bulk of our test.
		var wg sync.WaitGroup
		resolveErr := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, err := resolver.Resolve()
			if err != nil {
				resolveErr <- err
			}
		}()

		// At the output isn't yet in the nursery, we expect that we
		// should receive an incubation request.
		select {
		case <-incubateChan:
		case err := <-resolveErr:
			t.Fatalf("unable to resolve HTLC: %v", err)
		case <-time.After(time.Second * 5):
			t.Fatalf("failed to receive incubation request")
		}

		// Next, the resolver should request a spend notification for
		// the direct HTLC output. We'll use the txToBroadcast closure
		// for the test case to generate the transaction that we'll
		// send to the resolver.
		spendingTx, err := testCase.txToBroadcast()
		if err != nil {
			t.Fatalf("unable to generate tx: %v", err)
		}
		spendTxHash := spendingTx.TxHash()

		select {
		case notifier.SpendChan <- &chainntnfs.SpendDetail{
			SpendingTx:    spendingTx,
			SpenderTxHash: &spendTxHash,
		}:
		case <-time.After(time.Second * 5):
			t.Fatalf("failed to request spend ntfn")
		}

		if !testCase.timeout {
			// If the resolver should settle now, then we'll
			// extract the pre-image to be extracted and the
			// resolution message sent.
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
			case resolutionMsg := <-resolutionChan:
				// Once again, the pre-images should match up.
				if *resolutionMsg.PreImage != fakePreimage {
					t.Fatalf("wrong pre-image: "+
						"expected %v, got %v",
						fakePreimage, resolutionMsg.PreImage)
				}
			case <-time.After(time.Second * 5):
				t.Fatalf("resolution not sent")
			}
		} else {

			// Otherwise, the HTLC should now timeout.  First, we
			// should get a resolution message with a populated
			// failure message.
			select {
			case resolutionMsg := <-resolutionChan:
				if resolutionMsg.Failure == nil {
					t.Fatalf("expected failure resolution msg")
				}
			case <-time.After(time.Second * 5):
				t.Fatalf("resolution not sent")
			}

			// We should also get another request for the spend
			// notification of the second-level transaction to
			// indicate that it's been swept by the nursery, but
			// only if this is a local commitment transaction.
			if !testCase.remoteCommit {
				select {
				case notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    spendingTx,
					SpenderTxHash: &spendTxHash,
				}:
				case <-time.After(time.Second * 5):
					t.Fatalf("failed to request spend ntfn")
				}
			}
		}

		// In any case, before the resolver exits, it should checkpoint
		// its final state.
		select {
		case <-checkPointChan:
		case err := <-resolveErr:
			t.Fatalf("unable to resolve HTLC: %v", err)
		case <-time.After(time.Second * 5):
			t.Fatalf("check point not received")
		}

		// Add a report to our set of expected reports with the outcome
		// that the test specifies (either success or timeout).
		spendTxID := spendingTx.TxHash()
		amt := btcutil.Amount(fakeSignDesc.Output.Value)

		reports = append(reports, &channeldb.ResolverReport{
			OutPoint:        testChanPoint2,
			Amount:          amt,
			ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
			ResolverOutcome: testCase.outcome,
			SpendTxID:       &spendTxID,
		})

		for _, report := range reports {
			assertResolverReport(t, reportChan, report)
		}

		wg.Wait()

		// Finally, the resolver should be marked as resolved.
		if !resolver.resolved {
			t.Fatalf("resolver should be marked as resolved")
		}
	}
}

// NOTE: the following tests essentially checks many of the same scenarios as
// the test above, but they expand on it by checking resuming from checkpoints
// at every stage.

// TestHtlcTimeoutSingleStage tests a remote commitment confirming, and the
// local node sweeping the HTLC output directly after timeout.
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
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    sweepTx,
					SpenderTxHash: &sweepTxid,
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

	checkpoints := []checkpoint{
		{
			// Output should be handed off to the nursery.
			incubating: true,
		},
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {
				// The nursery will publish the timeout tx.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    timeoutTx,
					SpenderTxHash: &timeoutTxid,
				}

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

				// Deliver spend of timeout tx.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    sweepTx,
					SpenderTxHash: &sweepHash,
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// reports.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				firstStage, secondState,
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
			// Output should be handed off to the nursery.
			incubating: true,
		},
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
			incubating: true,
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
func TestHtlcTimeoutSecondStageSweeper(t *testing.T) {
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
	reSignedOutPoint := wire.OutPoint{
		Hash:  reSignedHash,
		Index: 1,
	}

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
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &reSignedHash,
	}

	secondState := &channeldb.ResolverReport{
		OutPoint:        reSignedOutPoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &sweepHash,
	}

	checkpoints := []checkpoint{
		{
			// The output should be given to the sweeper.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				resolver := ctx.resolver.(*htlcTimeoutResolver)
				inp := <-resolver.Sweeper.(*mockSweeper).sweptInputs
				op := inp.OutPoint()
				if *op != commitOutpoint {
					return fmt.Errorf("outpoint %v swept, "+
						"expected %v", op,
						commitOutpoint)
				}

				// Emulat the sweeper spending using the
				// re-signed timeout tx.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:        reSignedTimeoutTx,
					SpenderInputIndex: 1,
					SpenderTxHash:     &reSignedHash,
					SpendingHeight:    10,
				}

				return nil
			},
			// incubating=true is used to signal that the
			// second-level transaction was confirmed.
			incubating: true,
		},
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				resumed bool) error {

				// If we are resuming from a checkpoint, we
				// expect the resolver to re-subscribe to a
				// spend, hence we must resend it.
				if resumed {
					ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
						SpendingTx:        reSignedTimeoutTx,
						SpenderInputIndex: 1,
						SpenderTxHash:     &reSignedHash,
						SpendingHeight:    10,
					}
				}

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

				// Mimic CSV lock expiring.
				ctx.notifier.EpochChan <- &chainntnfs.BlockEpoch{
					Height: 13,
				}

				// The timeout tx output should now be given to
				// the sweeper.
				resolver := ctx.resolver.(*htlcTimeoutResolver)
				inp := <-resolver.Sweeper.(*mockSweeper).sweptInputs
				op := inp.OutPoint()
				exp := wire.OutPoint{
					Hash:  reSignedHash,
					Index: 1,
				}
				if *op != exp {
					return fmt.Errorf("wrong outpoint swept")
				}

				// Notify about the spend, which should resolve
				// the resolver.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:     sweepTx,
					SpenderTxHash:  &sweepHash,
					SpendingHeight: 14,
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// reports.
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				firstStage,
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
			// The output should be given to the sweeper.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				resolver := ctx.resolver.(*htlcTimeoutResolver)
				inp := <-resolver.Sweeper.(*mockSweeper).sweptInputs
				op := inp.OutPoint()
				if *op != commitOutpoint {
					return fmt.Errorf("outpoint %v swept, "+
						"expected %v", op,
						commitOutpoint)
				}

				// Emulate the remote sweeping the output with the preimage.
				// re-signed timeout tx.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    spendTx,
					SpenderTxHash: &spendTxHash,
				}

				return nil
			},
			// incubating=true is used to signal that the
			// second-level transaction was confirmed.
			incubating: true,
		},
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
			incubating: true,
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
			return &htlcTimeoutResolver{
				contractResolverKit: *newContractResolverKit(cfg),
				htlc:                htlc,
				htlcResolution:      resolution,
			}
		},
	)

	checkpointedState := runFromCheckpoint(t, ctx, checkpoints)

	// Now, from every checkpoint created, we re-create the resolver, and
	// run the test from that checkpoint.
	for i := range checkpointedState {
		cp := bytes.NewReader(checkpointedState[i])
		ctx := newHtlcResolverTestContext(t,
			func(htlc channeldb.HTLC, cfg ResolverConfig) ContractResolver {
				resolver, err := newTimeoutResolverFromReader(cp, cfg)
				if err != nil {
					t.Fatal(err)
				}

				resolver.Supplement(htlc)
				resolver.htlcResolution = resolution
				return resolver
			},
		)

		// Run from the given checkpoint, ensuring we'll hit the rest.
		_ = runFromCheckpoint(t, ctx, checkpoints[i+1:])
	}
}
