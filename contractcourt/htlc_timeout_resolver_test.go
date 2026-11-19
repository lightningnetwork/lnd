package contractcourt

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	testifymock "github.com/stretchr/testify/mock"
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

type htlcTimeoutTestCase struct {
	// name is a human readable description of the test case.
	name string

	// remoteCommit denotes if the commitment broadcast was the remote
	// commitment or not.
	remoteCommit bool

	// timeout denotes if the HTLC should be let timeout, or if the "remote"
	// party should sweep it on-chain. This also affects what type of
	// resolution message we expect.
	timeout bool

	// txToBroadcast is a function closure that should generate the
	// transaction that should spend the HTLC output. Test authors can use
	// this to customize the witness used when spending to trigger various
	// redemption cases.
	txToBroadcast func() (*wire.MsgTx, error)

	// outcome is the resolver outcome that we expect to be reported once
	// the contract is fully resolved.
	outcome channeldb.ResolverOutcome
}

func genHtlcTimeoutTestCases() []htlcTimeoutTestCase {
	fakePreimageBytes := testResPreimage[:]

	var (
		htlcOutpoint = testChanPoint2
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

	return []htlcTimeoutTestCase{
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
					templateTx.TxIn[0].Witness,
					witness,
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
					templateTx.TxIn[0].Witness,
					witness,
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
					templateTx.TxIn[0].Witness,
					witness,
				) {

					return templateTx, nil
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
			outcome: channeldb.ResolverOutcomeClaimed,
		},
	}
}

func testHtlcTimeoutResolver(t *testing.T, testCase htlcTimeoutTestCase) {
	fakePreimageBytes := testResPreimage[:]
	var fakePreimage lntypes.Preimage

	fakeSignDesc := &input.SignDescriptor{
		Output: &wire.TxOut{},
	}

	copy(fakePreimage[:], fakePreimageBytes)

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}

	witnessBeacon := newMockWitnessBeacon()
	checkPointChan := make(chan struct{}, 1)
	incubateChan := make(chan struct{}, 1)
	resolutionChan := make(chan ResolutionMsg, 1)
	reportChan := make(chan *channeldb.ResolverReport)

	//nolint:ll
	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:   notifier,
			Sweeper:    newMockSweeper(),
			PreimageDB: witnessBeacon,
			IncubateOutputs: func(wire.OutPoint,
				fn.Option[lnwallet.OutgoingHtlcResolution],
				fn.Option[lnwallet.IncomingHtlcResolution],
				uint32, fn.Option[int32],
				...IncubateOption) error {

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
			Budget: *DefaultBudgetConfig(),
			QueryIncomingCircuit: func(circuit models.CircuitKey,
			) *models.CircuitKey {

				return nil
			},
			HtlcNotifier: &mockHTLCNotifier{},
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
		// The hash has to correspond to the preimage the spending
		// witnesses reveal, since a claim is only accepted when the
		// revealed preimage actually opens this HTLC.
		htlc: channeldb.HTLC{
			Amt:   testHtlcAmt,
			RHash: testResHash,
		},
	}
	resolver.initLogger("timeoutResolver")

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
			report := &channeldb.ResolverReport{
				OutPoint:        timeoutTx.TxIn[0].PreviousOutPoint, //nolint:ll
				Amount:          testHtlcAmt.ToSatoshis(),
				ResolverType:    channeldb.ResolverTypeOutgoingHtlc,  //nolint:ll
				ResolverOutcome: channeldb.ResolverOutcomeFirstStage, //nolint:ll
				SpendTxID:       &timeoutTxID,
			}

			reports = append(reports, report)
		}
	}

	// With all the setup above complete, we can initiate the
	// resolution process, and the bulk of our test.
	var wg sync.WaitGroup
	resolveErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()

		err := resolver.Launch()
		if err != nil {
			resolveErr <- err
		}

		_, err = resolver.Resolve()
		if err != nil {
			resolveErr <- err
		}
	}()

	// If this is a remote commit, then we expct the outputs should receive
	// an incubation request to go through the sweeper, otherwise the
	// nursery.
	var sweepChan chan input.Input
	if testCase.remoteCommit {
		mockSweeper, ok := resolver.Sweeper.(*mockSweeper)
		require.True(t, ok)
		sweepChan = mockSweeper.sweptInputs
	}

	// The output should be offered to either the sweeper or the nursery.
	select {
	case <-incubateChan:
	case <-sweepChan:
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
	if testCase.remoteCommit {
		spendingTx.TxIn[0].PreviousOutPoint = testChanPoint2
	}
	spendTxHash := spendingTx.TxHash()

	select {
	case notifier.SpendChan <- &chainntnfs.SpendDetail{
		SpendingTx:    spendingTx,
		SpenderTxHash: &spendTxHash,
		SpentOutPoint: &testChanPoint2,
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
				SpentOutPoint: &testChanPoint2,
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
	if !resolver.resolved.Load() {
		t.Fatalf("resolver should be marked as resolved")
	}
}

// TestHtlcTimeoutResolver tests that the timeout resolver properly handles all
// variations of possible local+remote spends.
func TestHtlcTimeoutResolver(t *testing.T) {
	t.Parallel()

	testCases := genHtlcTimeoutTestCases()

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testHtlcTimeoutResolver(t, testCase)
		})
	}
}

// NOTE: the following tests essentially checks many of the same scenarios as
// the test above, but they expand on it by checking resuming from checkpoints
// at every stage.

// TestHtlcTimeoutSingleStage tests a remote commitment confirming, and the
// local node sweeping the HTLC output directly after timeout.
//
//nolint:ll
func TestHtlcTimeoutSingleStage(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 3}

	sweepTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: commitOutpoint,
		}},
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
			incubating: false,
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
//nolint:ll
func TestHtlcTimeoutSecondStagex(t *testing.T) {
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
					// Deliver spend of timeout tx.
					ctx.notifier.SpendChan <- timeoutSpend

					// Deliver spend of timeout tx output.
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
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: commitOutpoint,
		}},
		TxOut: []*wire.TxOut{{}},
	}

	fakePreimageBytes := testResPreimage[:]
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

// TestHtlcTimeoutSecondStageRemoteSpend tests that when a remote commitment
// confirms, and the remote spends the output using the success tx, we properly
// detect this and extract the preimage.
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

	fakePreimageBytes := testResPreimage[:]
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
			incubating: false,
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
//nolint:ll
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
	spendTx.TxIn[0].PreviousOutPoint = commitOutpoint

	fakePreimageBytes := testResPreimage[:]
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

	t.Helper()

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

	// annexBytes is a minimal but valid BIP341 annex: a single element
	// leading with the 0x50 tag.
	annexBytes := []byte{txscript.TaprootAnnexTag}

	testCases := []struct {
		name        string
		witness     wire.TxWitness
		isTaproot   bool
		localCommit bool
		expected    bool
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
			expected:    true,
		},
		{
			// The same spend with an annex appended. The spender
			// chooses whether to include one, so it must not
			// change how we classify the spend.
			name: "tap preimage spend on remote with annex",
			witness: wire.TxWitness{
				dummyBytes, dummyBytes, preimageBytes,
				dummyBytes, dummyBytes, annexBytes,
			},
			isTaproot:   true,
			localCommit: false,
			expected:    true,
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
			expected:    true,
		},
		{
			name: "tap preimage spend on local with annex",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes,
				dummyBytes, dummyBytes, annexBytes,
			},
			isTaproot:   true,
			localCommit: true,
			expected:    true,
		},
		{
			// An annex must not turn a key spend into something
			// that looks like a preimage spend.
			name:        "tap key spend with annex",
			witness:     wire.TxWitness{dummyBytes, annexBytes},
			isTaproot:   true,
			localCommit: true,
			expected:    false,
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
			expected:    true,
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
			expected:    true,
		},
		{
			// The annex is a taproot concept only. On a legacy
			// spend the final element is the witness script, so a
			// script that happens to lead with 0x50 must be left
			// on the stack.
			name: "legacy witness script leading with annex tag",
			witness: wire.TxWitness{
				dummyBytes, preimageBytes, annexBytes,
			},
			isTaproot:   false,
			localCommit: true,
			expected:    true,
		},
	}

	for _, tc := range testCases {

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
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestClaimCleanUpTaprootBreachAnnex tests that an annex on a taproot key
// spend doesn't hide the fact that we're on the losing side of a breach. The
// key spend path is a lone signature, so the annex has to come off before the
// stack is counted.
func TestClaimCleanUpTaprootBreachAnnex(t *testing.T) {
	t.Parallel()

	// A v1 witness program, so the resolver treats this as taproot.
	taprootPkScript := append(
		[]byte{txscript.OP_1, 0x20},
		bytes.Repeat([]byte{1}, lntypes.HashSize)...,
	)

	// A local commitment, so claimCleanUp reaches the breach check rather
	// than the remote sweep cases above it.
	resolver := &htlcTimeoutResolver{
		htlcResolution: lnwallet.OutgoingHtlcResolution{
			SweepSignDesc: input.SignDescriptor{
				Output: &wire.TxOut{PkScript: taprootPkScript},
			},
			SignedTimeoutTx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{}},
			},
		},
		htlc: channeldb.HTLC{RHash: testResHash},
	}

	// A key spend carrying an annex. The annex is preimage sized, so
	// without stripping it would be read as the preimage.
	spendingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			Witness: wire.TxWitness{
				dummyBytes,
				append(
					[]byte{txscript.TaprootAnnexTag},
					bytes.Repeat([]byte{7}, 31)...,
				),
			},
		}},
	}
	spend := &chainntnfs.SpendDetail{SpendingTx: spendingTx}

	err := resolver.claimCleanUp(spend)
	require.ErrorContains(t, err, "breach attempt failed")
	require.False(t, resolver.IsResolved())
}

// TestClaimCleanUpPreimageMismatch tests that a witness which merely has the
// shape of a success spend cannot inject a foreign preimage. The classifier
// only asserts the element at the preimage index is 32 bytes, so claimCleanUp
// has to confirm the preimage actually opens this HTLC.
func TestClaimCleanUpPreimageMismatch(t *testing.T) {
	t.Parallel()

	var preimage lntypes.Preimage
	copy(preimage[:], preimageBytes)

	// Build a resolver whose HTLC is locked to an entirely different
	// payment hash than the one the spending witness reveals.
	var otherHash lntypes.Hash
	copy(otherHash[:], bytes.Repeat([]byte{9}, lntypes.HashSize))

	resolver := &htlcTimeoutResolver{
		htlcResolution: lnwallet.OutgoingHtlcResolution{
			SweepSignDesc: input.SignDescriptor{
				Output: &wire.TxOut{},
			},
		},
		htlc: channeldb.HTLC{RHash: otherHash},
	}

	// A remote-commitment success spend on a legacy channel, carrying a
	// well-formed preimage for some other payment.
	spendingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			Witness: wire.TxWitness{
				dummyBytes, dummyBytes, dummyBytes,
				preimageBytes, dummyBytes,
			},
		}},
	}
	spend := &chainntnfs.SpendDetail{SpendingTx: spendingTx}

	// The witness passes the shape check, so this is exactly the input
	// claimCleanUp would be handed in practice.
	require.True(t, isPreimageSpend(false, spend, false))

	err := resolver.claimCleanUp(spend)
	require.ErrorIs(t, err, errPreimageMismatch)

	// Nothing should have been resolved off the back of a foreign
	// preimage.
	require.False(t, resolver.IsResolved())
}

// taprootHtlcVariant selects a realistic auxiliary-leaf topology for a test.
// Each value exercises a distinct proof-identity or tree-layout boundary.
type taprootHtlcVariant uint8

const (
	// taprootHtlcNoAux isolates the canonical two-leaf HTLC tree.
	taprootHtlcNoAux taprootHtlcVariant = iota
	// taprootHtlcUnrelatedAux adds a distinct committed third leaf.
	taprootHtlcUnrelatedAux
	// taprootHtlcDuplicateTimeout exposes timeout-proof hash ambiguity.
	taprootHtlcDuplicateTimeout
	// taprootHtlcDuplicateSuccess exposes indistinguishable success leaves.
	taprootHtlcDuplicateSuccess
	// taprootHtlcDifferentVersionSuccess reuses the success script under a
	// distinct leaf version so authentication cannot match by script alone.
	taprootHtlcDifferentVersionSuccess
)

// taprootHtlcFixture binds an actual HTLC tree to its resolver-owned data.
// This lets tests compare candidate proofs with the authoritative output.
type taprootHtlcFixture struct {
	resolver          *htlcTimeoutResolver
	tree              *input.HtlcScriptTree
	commitmentScript  []byte
	watchedOutpoint   wire.OutPoint
	timeoutScript     []byte
	timeoutControl    []byte
	successScript     []byte
	successControl    []byte
	localCommit       bool
	revocationKey     *btcec.PublicKey
	timeoutProofIndex int
}

// newTaprootHtlcFixture creates an actual sender or receiver HTLC tree and
// resolution data matching the commitment side under test.
func newTaprootHtlcFixture(t *testing.T, localCommit bool,
	variant taprootHtlcVariant) *taprootHtlcFixture {

	t.Helper()
	_, senderKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{2}, 32))
	_, receiverKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	_, revocationKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{4}, 32))
	buildTree := func(aux input.AuxTapLeaf) *input.HtlcScriptTree {
		var (
			tree *input.HtlcScriptTree
			err  error
		)
		if localCommit {
			tree, err = input.SenderHTLCScriptTaproot(
				senderKey, receiverKey, revocationKey,
				testResHash[:], lntypes.Local, aux,
			)
		} else {
			tree, err = input.ReceiverHTLCScriptTaproot(
				500, senderKey, receiverKey, revocationKey,
				testResHash[:], lntypes.Remote, aux,
			)
		}
		require.NoError(t, err)

		return tree
	}
	baseTree := buildTree(input.NoneTapLeaf())
	auxLeaf := input.NoneTapLeaf()
	switch variant {
	case taprootHtlcUnrelatedAux:
		auxLeaf = fn.Some(txscript.NewBaseTapLeaf([]byte{
			txscript.OP_TRUE,
		}))
	case taprootHtlcDuplicateTimeout:
		auxLeaf = fn.Some(baseTree.TimeoutTapLeaf)
	case taprootHtlcDuplicateSuccess:
		auxLeaf = fn.Some(baseTree.SuccessTapLeaf)

	case taprootHtlcDifferentVersionSuccess:
		auxLeaf = fn.Some(txscript.NewTapLeaf(
			txscript.TapscriptLeafVersion(0xc2),
			baseTree.SuccessTapLeaf.Script,
		))
	}
	tree := buildTree(auxLeaf)
	commitmentScript, err := input.PayToTaprootScript(tree.TaprootKey)
	require.NoError(t, err)
	timeoutControl, err := tree.CtrlBlockForPath(input.ScriptPathTimeout)
	require.NoError(t, err)
	timeoutControlBytes, err := timeoutControl.ToBytes()
	require.NoError(t, err)
	successIndex := 1
	timeoutIndex := 0
	if localCommit {
		successIndex = 0
		timeoutIndex = 1
	}
	successControl := tree.TapscriptTree.LeafMerkleProofs[successIndex].
		ToControlBlock(revocationKey)
	successControlBytes, err := successControl.ToBytes()
	require.NoError(t, err)

	watchedOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{byte(variant + 1)},
		Index: uint32(timeoutIndex),
	}
	resolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint: watchedOutpoint,
		SweepSignDesc: input.SignDescriptor{
			Output:        &wire.TxOut{PkScript: commitmentScript},
			WitnessScript: tree.TimeoutTapLeaf.Script,
			ControlBlock:  timeoutControlBytes,
		},
	}
	if localCommit {
		secondLevelScript, err := input.PayToTaprootScript(senderKey)
		require.NoError(t, err)
		resolution.SignedTimeoutTx = &wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: watchedOutpoint,
				Witness: wire.TxWitness{
					dummyBytes, dummyBytes,
					tree.TimeoutTapLeaf.Script,
					timeoutControlBytes,
				},
			}},
		}
		resolution.SignDetails = &input.SignDetails{
			SignDesc: input.SignDescriptor{
				Output: &wire.TxOut{PkScript: commitmentScript},
			},
		}
		// A local SweepSignDesc describes the second-level output, not
		// the commitment HTLC output selected by the helper.
		resolution.SweepSignDesc.Output = &wire.TxOut{
			PkScript: secondLevelScript,
		}
	}

	return &taprootHtlcFixture{
		resolver: &htlcTimeoutResolver{
			htlcResolution: resolution,
			htlc:           channeldb.HTLC{RHash: testResHash},
		},
		tree:              tree,
		commitmentScript:  commitmentScript,
		watchedOutpoint:   watchedOutpoint,
		timeoutScript:     tree.TimeoutTapLeaf.Script,
		timeoutControl:    timeoutControlBytes,
		successScript:     tree.SuccessTapLeaf.Script,
		successControl:    successControlBytes,
		localCommit:       localCommit,
		revocationKey:     revocationKey,
		timeoutProofIndex: timeoutIndex,
	}
}

// TestTaprootHtlcCommitmentScript tests that commitment scripts come from
// authoritative resolution outputs for both commitment sides.
func TestTaprootHtlcCommitmentScript(t *testing.T) {
	// Arrange canonical, auxiliary, and duplicate-leaf trees so both
	// commitment sides and every authoritative data source are covered.
	variants := []taprootHtlcVariant{
		taprootHtlcNoAux, taprootHtlcUnrelatedAux,
		taprootHtlcDuplicateTimeout, taprootHtlcDuplicateSuccess,
	}
	for _, localCommit := range []bool{true, false} {
		for _, variant := range variants {
			// Act by reading the stored commitment output rather
			// than rebuilding a program from candidate proof data.
			fixture := newTaprootHtlcFixture(
				t, localCommit, variant,
			)
			name := fmt.Sprintf("local=%v/variant=%v",
				localCommit, variant)
			t.Run(name, func(t *testing.T) {
				script, err := fixture.resolver.
					taprootHtlcCommitmentScript()
				// Assert the program is returned and state
				// remains unchanged; error cases follow below.
				require.NoError(t, err)
				require.Equal(
					t, fixture.commitmentScript, script,
				)
				require.False(t, fixture.resolver.IsResolved())
			})
		}
	}

	local := newTaprootHtlcFixture(t, true, taprootHtlcNoAux)
	remote := newTaprootHtlcFixture(t, false, taprootHtlcNoAux)
	testCases := []struct {
		name     string
		resolver *htlcTimeoutResolver
	}{
		{
			name: "missing local sign details",
			resolver: func() *htlcTimeoutResolver {
				local.resolver.htlcResolution.SignDetails = nil
				return local.resolver
			}(),
		},
		{
			name: "missing remote output",
			resolver: func() *htlcTimeoutResolver {
				remote.resolver.htlcResolution.SweepSignDesc.
					Output = nil
				return remote.resolver
			}(),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			script, err := testCase.resolver.
				taprootHtlcCommitmentScript()

			require.Error(t, err)
			require.Nil(t, script)
			require.False(t, testCase.resolver.IsResolved())
		})
	}

	for _, localCommit := range []bool{true, false} {
		for _, missingOutput := range []bool{true, false} {
			fixture := newTaprootHtlcFixture(
				t, localCommit, taprootHtlcNoAux,
			)
			switch {
			case localCommit && missingOutput:
				fixture.resolver.htlcResolution.SignDetails.
					SignDesc.Output = nil

			case localCommit:
				output := fixture.resolver.htlcResolution.
					SignDetails.SignDesc.Output
				output.PkScript = nil

			case missingOutput:
				fixture.resolver.htlcResolution.
					SweepSignDesc.Output = nil

			default:
				fixture.resolver.htlcResolution.
					SweepSignDesc.Output.PkScript = nil
			}

			_, err := fixture.resolver.taprootHtlcCommitmentScript()
			require.Error(t, err)
		}
	}
}

// setTaprootTimeoutControl replaces the stored timeout proof on either
// commitment side of a fixture.
func setTaprootTimeoutControl(fixture *taprootHtlcFixture, control []byte) {
	resolution := &fixture.resolver.htlcResolution
	if fixture.localCommit {
		witness := resolution.SignedTimeoutTx.TxIn[0].Witness
		witness[len(witness)-1] = control
		return
	}

	resolution.SweepSignDesc.ControlBlock = control
}

// TestCanonicalTaprootSuccessHashes tests independent timeout hashing and the
// optional identity obtained from a verified stored proof.
func TestCanonicalTaprootSuccessHashes(t *testing.T) {
	// Arrange: Cover both commitment owners and every supported auxiliary
	// leaf topology, then corrupt the stored proof in controlled ways.
	// Act: Ask the resolver to derive identities from each authoritative
	// commitment, including the deliberately damaged proof variants.
	// Assert: Valid trees expose only unambiguous identities, while absent
	// or unauthenticated proofs never contribute a success-leaf sibling.
	variants := []taprootHtlcVariant{
		taprootHtlcNoAux, taprootHtlcUnrelatedAux,
		taprootHtlcDuplicateTimeout, taprootHtlcDuplicateSuccess,
	}
	for _, localCommit := range []bool{true, false} {
		for _, variant := range variants {
			fixture := newTaprootHtlcFixture(
				t, localCommit, variant,
			)
			name := fmt.Sprintf("local=%v/variant=%v",
				localCommit, variant)
			t.Run(name, func(t *testing.T) {
				identities, err := fixture.resolver.
					canonicalTaprootSuccessHashes()
				require.NoError(t, err)
				require.Equal(t,
					txscript.NewBaseTapLeaf(
						fixture.timeoutScript,
					).TapHash(),
					identities.timeoutLeafHash,
				)
				if variant == taprootHtlcDuplicateTimeout {
					sibling := identities.storedProofSibling
					missing := sibling.IsNone()
					require.True(t, missing)

					return
				}
				require.Equal(t,
					fixture.tree.SuccessTapLeaf.TapHash(),
					identities.storedProofSibling.
						UnwrapOrFail(t),
				)
			})
		}
	}
	for _, localCommit := range []bool{true, false} {
		for _, mutation := range []string{
			"malformed proof", "siblingless proof",
			"different commitment",
		} {
			fixture := newTaprootHtlcFixture(
				t, localCommit, taprootHtlcUnrelatedAux,
			)
			switch mutation {
			case "malformed proof":
				setTaprootTimeoutControl(fixture, []byte{1})
			case "siblingless proof":
				control, err := txscript.ParseControlBlock(
					fixture.timeoutControl,
				)
				require.NoError(t, err)
				control.InclusionProof = nil
				controlBytes, err := control.ToBytes()
				require.NoError(t, err)
				setTaprootTimeoutControl(fixture, controlBytes)

			case "different commitment":
				other := newTaprootHtlcFixture(
					t, localCommit, taprootHtlcNoAux,
				)
				resolution := &fixture.resolver.htlcResolution
				if localCommit {
					resolution.SignDetails.
						SignDesc.Output.PkScript =
						other.commitmentScript
				} else {
					resolution.SweepSignDesc.Output.
						PkScript =
						other.commitmentScript
				}
			}
			t.Run(fmt.Sprintf("local=%v/%s", localCommit,
				mutation), func(t *testing.T) {
				identities, err := fixture.resolver.
					canonicalTaprootSuccessHashes()
				require.NoError(t, err)
				require.True(t,
					identities.storedProofSibling.IsNone())
			})
		}
	}

	for _, localCommit := range []bool{true, false} {
		fixture := newTaprootHtlcFixture(
			t, localCommit, taprootHtlcNoAux,
		)
		if localCommit {
			fixture.resolver.htlcResolution.SignedTimeoutTx.
				TxIn[0].Witness = nil
		} else {
			fixture.resolver.htlcResolution.SweepSignDesc.
				WitnessScript = nil
		}

		_, err := fixture.resolver.canonicalTaprootSuccessHashes()
		require.Error(t, err)
	}
}

// TestTaprootPreimageSpend authenticates committed success candidates.
//
//nolint:ll
func TestTaprootPreimageSpend(t *testing.T) {
	// Arrange: Build cases spanning valid success proofs, other committed
	// paths, malformed witnesses, mismatched preimages, and wrong inputs.
	testCases := []struct {
		name    string
		local   bool
		variant taprootHtlcVariant
		path    string
		annex   bool
		want    bool
		wantErr string
	}{
		{name: "local success", local: true, want: true},
		{name: "local success with annex", local: true, annex: true, want: true},
		{name: "remote success", want: true},
		{name: "remote success with annex", annex: true, want: true},
		{name: "unrelated auxiliary", local: true,
			variant: taprootHtlcUnrelatedAux, path: "aux"},
		{name: "different leaf version", variant: taprootHtlcDifferentVersionSuccess, path: "aux"},
		{name: "timeout leaf", local: true, path: "timeout"},
		{name: "local duplicate timeout", local: true,
			variant: taprootHtlcDuplicateTimeout, want: true},
		{name: "remote duplicate timeout",
			variant: taprootHtlcDuplicateTimeout, want: true},
		{name: "local duplicate success", local: true,
			variant: taprootHtlcDuplicateSuccess, path: "duplicate", want: true},
		{name: "remote duplicate success", variant: taprootHtlcDuplicateSuccess,
			path: "duplicate", want: true},
		{name: "key path", local: true, path: "key"},
		{name: "key path with annex", local: true, path: "key", annex: true},
		{name: "empty witness", path: "empty", wantErr: "malformed"},
		{name: "corrupt control", local: true, path: "corrupt",
			wantErr: "malformed"},
		{name: "short preimage", local: true, path: "short",
			wantErr: "malformed"},
		{name: "wrong preimage", path: "wrong", wantErr: "mismatch"},
		{name: "wrong outpoint", local: true, path: "outpoint",
			wantErr: "spend"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTaprootHtlcFixture(t, testCase.local, testCase.variant)
			script, control := fixture.successScript, fixture.successControl
			switch testCase.path {
			case "aux":
				proof := fixture.tree.TapscriptTree.LeafMerkleProofs[2]
				block := proof.ToControlBlock(fixture.revocationKey)
				script = proof.Script
				controlBytes, err := block.ToBytes()
				require.NoError(t, err)
				control = controlBytes
			case "timeout":
				script, control = fixture.timeoutScript, fixture.timeoutControl
			case "duplicate":
				branch := txscript.NewTapBranch(
					fixture.tree.SuccessTapLeaf,
					fixture.tree.TimeoutTapLeaf,
				).TapHash()
				proof := txscript.TapscriptProof{
					TapLeaf:        fixture.tree.SuccessTapLeaf,
					RootNode:       fixture.tree.TapscriptTree.RootNode,
					InclusionProof: branch[:],
				}
				block := proof.ToControlBlock(fixture.revocationKey)
				controlBytes, err := block.ToBytes()
				require.NoError(t, err)
				control = controlBytes
			case "corrupt":
				control = append([]byte(nil), control...)
				control[len(control)-1] ^= 1
			}
			preimage := testResPreimage[:]
			if testCase.path == "wrong" {
				preimage = bytes.Repeat([]byte{9}, lntypes.HashSize)
			}
			witness := wire.TxWitness{
				dummyBytes, dummyBytes, preimage, script, control,
			}
			if testCase.local {
				witness = wire.TxWitness{dummyBytes, preimage, script, control}
			}
			switch testCase.path {
			case "key":
				witness = wire.TxWitness{dummyBytes}
			case "empty":
				witness = nil
			case "short":
				witness[localPreimageIndex] = preimage[:31]
			}
			if testCase.annex {
				witness = append(witness, []byte{txscript.TaprootAnnexTag})
			}
			outpoint := fixture.watchedOutpoint
			if testCase.path == "outpoint" {
				outpoint.Index++
			}
			spend := &chainntnfs.SpendDetail{
				SpendingTx: &wire.MsgTx{TxIn: []*wire.TxIn{{
					PreviousOutPoint: outpoint, Witness: witness,
				}}},
			}
			// Act: Classify the fully assembled spend against the resolver's
			// authoritative outpoint, output key, leaf version, and script.
			result, err := fixture.resolver.isTaprootPreimageSpend(spend)
			// Assert: Each case distinguishes authenticated success claims
			// from benign other paths and malformed or dishonest candidates.
			switch testCase.wantErr {
			case "mismatch":
				require.ErrorIs(t, err, errPreimageMismatch)
			case "spend":
				require.ErrorIs(t, err, errInvalidSpendDetails)
			case "malformed":
				require.Error(t, err)
				require.NotErrorIs(t, err, errPreimageMismatch)
			default:
				require.NoError(t, err)
			}
			require.Equal(t, testCase.want, result)
			require.False(t, fixture.resolver.IsResolved())
		})
	}
}

// spendRegistration preserves the output identity passed to the notifier.
// Tests use this copy to verify the resolver watched authoritative data.
type spendRegistration struct {
	outpoint wire.OutPoint
	pkScript []byte
}

// newSpendMockNotifier configures the standard testify-backed notifier mock.
// Its explicit events let tests drive chain activity and inspect registrations.
func newSpendMockNotifier() (*chainntnfs.MockChainNotifier,
	chan *chainntnfs.SpendDetail, chan *chainntnfs.BlockEpoch,
	chan spendRegistration) {

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	epochChan := make(chan *chainntnfs.BlockEpoch, 1)
	// Two slots let a timeout resolver register both commitment and
	// second-level outputs when a test only needs to drive its events.
	registered := make(chan spendRegistration, 2)
	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On(
		"RegisterSpendNtfn", testifymock.Anything,
		testifymock.Anything, testifymock.Anything,
	).Run(func(args testifymock.Arguments) {
		outpoint, ok := args.Get(0).(*wire.OutPoint)
		if !ok {
			panic("mock spend outpoint has unexpected type")
		}
		pkScript, ok := args.Get(1).([]byte)
		if !ok {
			panic("mock spend script has unexpected type")
		}
		registered <- spendRegistration{
			outpoint: *outpoint,
			pkScript: append([]byte(nil), pkScript...),
		}
	}).Return(&chainntnfs.SpendEvent{
		Spend: spendChan,
		Cancel: func() {
		},
	}, nil)
	notifier.On(
		"RegisterBlockEpochNtfn", testifymock.Anything,
	).Return(&chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {
		},
	}, nil)

	return notifier, spendChan, epochChan, registered
}

// TestTaprootChainDetailsToWatch tests that local Taproot spend watches use
// the authoritative commitment output even when the timeout proof is unsafe.
func TestTaprootChainDetailsToWatch(t *testing.T) {
	// Arrange: Build valid and malformed local timeout proofs backed by an
	// authoritative commitment output, plus one missing-output failure.
	// Act: Derive each watch target and start the spend wait through the
	// testify-backed notifier so its exact registration can be observed.
	// Assert: Every usable resolution watches the stored output; missing
	// authoritative data fails without resolving the contract.
	testCases := []struct {
		name      string
		variant   taprootHtlcVariant
		malformed bool
	}{
		{"duplicate timeout", taprootHtlcDuplicateTimeout, false},
		{"malformed proof", taprootHtlcNoAux, true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTaprootHtlcFixture(
				t, true, testCase.variant,
			)
			if testCase.malformed {
				setTaprootTimeoutControl(fixture, []byte{1})
			}

			outpoint, script, err := fixture.resolver.
				chainDetailsToWatch()
			require.NoError(t, err)
			require.Equal(t, fixture.watchedOutpoint, *outpoint)
			require.Equal(t, fixture.commitmentScript, script)

			notifier, _, _, registered := newSpendMockNotifier()
			chainCfg := ChannelArbitratorConfig{
				ChainArbitratorConfig: ChainArbitratorConfig{
					Notifier: notifier,
				},
			}
			resolverCfg := ResolverConfig{
				ChannelArbitratorConfig: chainCfg,
			}
			fixture.resolver.contractResolverKit =
				*newContractResolverKit(resolverCfg)
			result := make(chan error, 1)
			go func() {
				_, err := fixture.resolver.watchHtlcSpend()
				result <- err
			}()

			registration := <-registered
			require.Equal(t, fixture.watchedOutpoint,
				registration.outpoint)
			require.Equal(t, fixture.commitmentScript,
				registration.pkScript)
			close(fixture.resolver.quit)
			require.ErrorIs(t, <-result, errResolverShuttingDown)
			require.False(t, fixture.resolver.IsResolved())
		})
	}

	fixture := newTaprootHtlcFixture(t, true, taprootHtlcNoAux)
	fixture.resolver.htlcResolution.SignDetails = nil
	_, _, err := fixture.resolver.chainDetailsToWatch()
	require.Error(t, err)
}

// TestHtlcOutgoingResolverTaprootRegistration tests that contest resolution
// registers the authoritative local commitment output before waiting.
func TestHtlcOutgoingResolverTaprootRegistration(t *testing.T) {
	// Arrange: Create duplicate-timeout trees with valid and malformed
	// proofs while keeping the commitment output authoritative.
	// Act: Run resolution through the testify notifier and capture its
	// registration before stopping the asynchronous wait.
	// Assert: Resolution always registers the exact HTLC output, then exits
	// unresolved with the expected shutdown result and no successor.
	for _, malformed := range []bool{false, true} {
		fixture := newTaprootHtlcFixture(
			t, true, taprootHtlcDuplicateTimeout,
		)
		if malformed {
			setTaprootTimeoutControl(fixture, []byte{1})
		}
		notifier, _, _, registered := newSpendMockNotifier()
		chainCfg := ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				Notifier: notifier,
			},
		}
		fixture.resolver.contractResolverKit = *newContractResolverKit(
			ResolverConfig{
				ChannelArbitratorConfig: chainCfg,
			},
		)
		resolver := &htlcOutgoingContestResolver{
			htlcTimeoutResolver: fixture.resolver,
		}
		result := make(chan resolveResult, 1)
		go func() {
			next, err := resolver.Resolve()
			result <- resolveResult{nextResolver: next, err: err}
		}()

		registration := <-registered
		require.Equal(t, fixture.watchedOutpoint, registration.outpoint)
		require.Equal(
			t, fixture.commitmentScript, registration.pkScript,
		)
		close(resolver.quit)
		resolved := <-result
		require.ErrorIs(t, resolved.err, errResolverShuttingDown)
		require.Nil(t, resolved.nextResolver)
		require.False(t, resolver.IsResolved())
	}
}

// taprootSpendForPath creates a notifier spend for a realistic fixture path.
func taprootSpendForPath(t *testing.T, fixture *taprootHtlcFixture,
	path string) *chainntnfs.SpendDetail {

	t.Helper()
	script, control := fixture.successScript, fixture.successControl
	preimage := testResPreimage[:]
	if path == "auxiliary" {
		proof := fixture.tree.TapscriptTree.LeafMerkleProofs[2]
		block := proof.ToControlBlock(fixture.revocationKey)
		var err error
		control, err = block.ToBytes()
		require.NoError(t, err)
		script = proof.Script
	}
	if path == "wrong" {
		preimage = bytes.Repeat([]byte{9}, lntypes.HashSize)
	}
	witness := wire.TxWitness{
		dummyBytes, dummyBytes, preimage, script, control,
	}
	if fixture.localCommit {
		witness = wire.TxWitness{dummyBytes, preimage, script, control}
	}
	if path == "key" {
		witness = wire.TxWitness{dummyBytes}
	}
	tx := &wire.MsgTx{TxIn: []*wire.TxIn{{
		PreviousOutPoint: fixture.watchedOutpoint,
		Witness:          witness,
	}}}
	txHash := tx.TxHash()

	return &chainntnfs.SpendDetail{
		SpentOutPoint:     &fixture.watchedOutpoint,
		SpendingTx:        tx,
		SpenderTxHash:     &txHash,
		SpenderInputIndex: 0,
	}
}

// TestHtlcTimeoutTaprootSpendPath tests every Taproot spend decision point.
//
//nolint:ll
func TestHtlcTimeoutTaprootSpendPath(t *testing.T) {
	// Arrange: Combine every resolver entry point with authenticated success,
	// benign alternate paths, and dishonest preimage witnesses.
	// Act: Deliver each spend through the same explicit notifier event or the
	// paired mempool/block streams used by the production decision path.
	// Assert: Only valid success paths reveal and persist a preimage, while
	// every other path preserves the resolver state appropriate to its site.
	testCases := []struct {
		name  string
		site  string
		path  string
		local bool
	}{
		{"confirmed success", "confirmed", "success", false},
		{"confirmed auxiliary", "confirmed", "auxiliary", true},
		{"mempool success", "mempool", "success", false},
		{"mempool wrong preimage", "mempool", "wrong", false},
		{"mempool key path", "mempool", "key", true},
		{"remote success", "remote", "success", false},
		{"remote auxiliary", "remote", "auxiliary", false},
		{"local success", "local", "success", true},
		{"local wrong preimage", "local", "wrong", true},
		{"local key path", "local", "key", true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			variant := taprootHtlcNoAux
			if testCase.path == "auxiliary" {
				variant = taprootHtlcUnrelatedAux
			}
			fixture := newTaprootHtlcFixture(
				t, testCase.local, variant,
			)
			spend := taprootSpendForPath(t, fixture, testCase.path)
			notifier, spendChan, _, _ := newSpendMockNotifier()
			beacon := newMockWitnessBeacon()
			resolutionChan := make(chan ResolutionMsg, 1)
			checkpointChan := make(chan struct{}, 1)
			chainCfg := ChannelArbitratorConfig{
				ChainArbitratorConfig: ChainArbitratorConfig{
					Notifier:   notifier,
					PreimageDB: beacon,
					DeliverResolutionMsg: func(...ResolutionMsg) error {
						resolutionChan <- ResolutionMsg{}
						return nil
					},
				},
			}
			fixture.resolver.contractResolverKit = *newContractResolverKit(
				ResolverConfig{
					ChannelArbitratorConfig: chainCfg,
					Checkpoint: func(ContractResolver,
						...*channeldb.ResolverReport) error {

						checkpointChan <- struct{}{}
						return nil
					},
				},
			)
			fixture.resolver.initLogger("htlcTimeoutResolver")
			fixture.resolver.currentReport = ContractReport{LimboBalance: 1}
			initialReport := fixture.resolver.currentReport
			var (
				returnedSpend *chainntnfs.SpendDetail
				err           error
			)
			switch testCase.site {
			case "confirmed":
				spendChan <- spend
				returnedSpend, err = fixture.resolver.
					waitHtlcSpendAndCheckPreimage()
			case "mempool":
				block := make(chan *chainntnfs.SpendDetail)
				mempool := make(chan *chainntnfs.SpendDetail)
				result := make(chan *spendResult, 1)
				go fixture.resolver.consumeSpendEvents(
					result, block, mempool,
				)
				mempool <- spend
				if testCase.path == "key" {
					close(block)
				}
				spendResult := <-result
				returnedSpend, err = spendResult.spend, spendResult.err
				close(fixture.resolver.quit)
			case "remote":
				spendChan <- spend
				err = fixture.resolver.resolveRemoteCommitOutput()
			case "local":
				spendChan <- spend
				close(spendChan)
				err = fixture.resolver.resolveTimeoutTx()
			}
			switch testCase.path {
			case "success":
				require.NoError(t, err)
				if testCase.site == "mempool" {
					require.Same(t, spend, returnedSpend)
					break
				}
				require.Nil(t, returnedSpend)
				require.Len(t, beacon.newPreimages, 1)
				require.Len(t, resolutionChan, 1)
				require.Len(t, checkpointChan, 1)
				require.True(t, fixture.resolver.IsResolved())

			case "wrong":
				require.ErrorIs(t, err, errPreimageMismatch)

			case "auxiliary":
				require.NoError(t, err)
				if testCase.site == "confirmed" {
					require.Same(t, spend, returnedSpend)
				}

			case "key":
				require.ErrorIs(t, err, errResolverShuttingDown)
			}

			if testCase.path != "success" {
				require.Empty(t, beacon.newPreimages)
				require.Equal(t, initialReport,
					fixture.resolver.currentReport)
				if testCase.site == "remote" {
					require.True(t, fixture.resolver.IsResolved())
					require.Len(t, resolutionChan, 1)
					require.Len(t, checkpointChan, 1)
				} else {
					require.False(t, fixture.resolver.IsResolved())
					require.Empty(t, resolutionChan)
					require.Empty(t, checkpointChan)
				}
			}
		})
	}
}
