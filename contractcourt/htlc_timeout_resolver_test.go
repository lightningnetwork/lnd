package contractcourt

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

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

type recordingSpendNotifier struct {
	chainntnfs.ChainNotifier
	registrations chan wire.OutPoint
}

// RegisterSpendNtfn records the watched outpoint before delegating to the
// shared test notifier.
func (r *recordingSpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	r.registrations <- *outpoint
	return r.ChainNotifier.RegisterSpendNtfn(outpoint, pkScript, heightHint)
}

func genHtlcTimeoutTestCases() []htlcTimeoutTestCase {
	fakePreimageBytes := testResPreimage[:]

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

	return []htlcTimeoutTestCase{
		// Remote commitment is broadcast, we time out the HTLC on
		// chain, and should expect a fail HTLC resolution.
		{
			name:         "timeout remote tx",
			remoteCommit: true,
			timeout:      true,
			txToBroadcast: func() (*wire.MsgTx, error) {
				templateTx.TxIn[0].PreviousOutPoint =
					testChanPoint2
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
				templateTx.TxIn[0].PreviousOutPoint =
					testChanPoint2
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

// TestHtlcTimeoutRejectsMalformedSpendEvent tests validation of notifier
// spending transaction inputs.
func TestHtlcTimeoutRejectsMalformedSpendEvent(t *testing.T) {
	// Arrange a valid watched outpoint and table mutations covering every
	// malformed notifier field consumed by validateSpend.
	htlcOutpoint := wire.OutPoint{Index: 3}
	validSpend := newSpendDetail(htlcOutpoint, &wire.MsgTx{
		TxIn: []*wire.TxIn{{PreviousOutPoint: htlcOutpoint}},
	}, 0)

	testCases := []struct {
		name  string
		spend *chainntnfs.SpendDetail
		valid bool
	}{
		{
			name:  "valid spend",
			spend: validSpend,
			valid: true,
		},
		{
			name: "missing detail",
		},
		{
			name:  "missing transaction",
			spend: &chainntnfs.SpendDetail{},
		},
		{
			name: "input index out of range",
			spend: &chainntnfs.SpendDetail{
				SpendingTx:        &wire.MsgTx{},
				SpenderInputIndex: 1,
			},
		},
		{
			name: "missing indexed input",
			spend: &chainntnfs.SpendDetail{
				SpendingTx: &wire.MsgTx{
					TxIn: []*wire.TxIn{nil},
				},
			},
		},
		{
			name: "unexpected previous outpoint",
			spend: newSpendDetail(htlcOutpoint, &wire.MsgTx{
				TxIn: []*wire.TxIn{{}},
			}, 0),
		},
	}

	resolver := &htlcTimeoutResolver{
		htlcResolution: lnwallet.OutgoingHtlcResolution{
			ClaimOutpoint: htlcOutpoint,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Act by validating the spend before a watcher can log,
			// classify, or forward its transaction.
			err := resolver.validateSpend(testCase.spend)

			// Assert valid metadata passes unchanged. Malformed
			// shapes retain the spend-details sentinel.
			if testCase.valid {
				require.NoError(t, err)
				return
			}

			require.ErrorIs(t, err, errInvalidSpendDetails)
		})
	}
}

// TestHtlcTimeoutConsumeSpendEvents tests that malformed block and mempool
// events are returned as errors before they are inspected further.
func TestHtlcTimeoutConsumeSpendEvents(t *testing.T) {
	// Arrange equivalent block and mempool cases so both subscription
	// branches receive malformed notifier data through their real channels.
	testCases := []struct {
		name    string
		mempool bool
	}{
		{name: "block"},
		{name: "mempool", mempool: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange channels and route a nil spend through only
			// the backend selected by this case.
			blockSpend := make(chan *chainntnfs.SpendDetail, 1)
			mempoolSpend := make(chan *chainntnfs.SpendDetail, 1)
			if testCase.mempool {
				mempoolSpend <- nil
			} else {
				blockSpend <- nil
			}

			resolver := &htlcTimeoutResolver{
				htlcResolution: lnwallet.OutgoingHtlcResolution{
					ClaimOutpoint: wire.OutPoint{Index: 3},
				},
			}
			resultChan := make(chan *spendResult, 1)

			// Act by consuming the malformed event through the loop
			// shared by the block and mempool subscriptions.
			resolver.consumeSpendEvents(
				resultChan, blockSpend, mempoolSpend,
			)

			// Assert the watcher returns validation failure without
			// forwarding a spend that later code could dereference.
			result := <-resultChan
			require.ErrorIs(t, result.err, errInvalidSpendDetails)
			require.Nil(t, result.spend)
		})
	}
}

// TestHtlcTimeoutRejectsMalformedConfirmedSpend tests that the confirmed-only
// watcher returns malformed remote commitment spends without side effects.
func TestHtlcTimeoutRejectsMalformedConfirmedSpend(t *testing.T) {
	// Arrange a confirmed-only resolver whose notifier reports a
	// transaction that does not spend the watched HTLC outpoint.
	htlcOutpoint := wire.OutPoint{Index: 3}
	ctx := newHtlcResolverTestContext(t, func(htlc channeldb.HTLC,
		cfg ResolverConfig) ContractResolver {

		resolution := lnwallet.OutgoingHtlcResolution{
			ClaimOutpoint: htlcOutpoint,
			SweepSignDesc: testSignDesc,
		}

		return newTimeoutResolver(resolution, 0, htlc, 0, cfg)
	})
	var checkpoints int
	ctx.checkpoint = func(_ ContractResolver,
		_ ...*channeldb.ResolverReport) error {

		checkpoints++
		return nil
	}
	ctx.notifier.SpendChan <- newSpendDetail(
		htlcOutpoint, &wire.MsgTx{TxIn: []*wire.TxIn{{}}}, 0,
	)

	resolver, ok := ctx.resolver.(*htlcTimeoutResolver)
	require.True(t, ok)

	// Act by running the remote-commit resolution path that waits directly
	// for the malformed confirmed notification.
	err := resolver.resolveRemoteCommitOutput()

	// Assert validation stops resolution before checkpoint, notification,
	// outcome persistence, or sweep submission can produce side effects.
	require.ErrorIs(t, err, errInvalidSpendDetails)
	require.False(t, resolver.IsResolved())
	require.Zero(t, checkpoints)
	require.Empty(t, ctx.resolutionChan)
	require.False(t, ctx.finalHtlcOutcomeStored)
	require.Empty(t, ctx.htlcNotifier.finalHtlcEvents)

	sweeper, ok := resolver.Sweeper.(*mockSweeper)
	require.True(t, ok)
	require.Empty(t, sweeper.sweptInputs)
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
	secondLevelOutput := cloneTxOut(testSignDesc.Output)
	secondLevelOutput.PkScript = []byte{0xff, 0xff}
	sweepSignDesc := testSignDesc
	sweepSignDesc.Output = secondLevelOutput

	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
		TxOut: []*wire.TxOut{cloneTxOut(secondLevelOutput)},
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
			cloneTxOut(secondLevelOutput),
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
		SweepSignDesc: sweepSignDesc,
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

	registrations := make(chan wire.OutPoint, 3)
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

				var watched wire.OutPoint
				for watched != timeoutTxOutpoint {
					select {
					case watched = <-registrations:
					case <-time.After(time.Second):
						t.Fatal("expected spend registration")
					}
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
		t, twoStageResolution, checkpoints, registrations,
	)
}

// testHtlcTimeoutOutputMatch builds a valid live zero-fee timeout spend, lets
// the caller mutate one output-matching condition, and verifies either clean
// rejection or terminal timeout handling without a phantom sweep.
func testHtlcTimeoutOutputMatch(t *testing.T,
	prepare func(*lnwallet.OutgoingHtlcResolution,
		*chainntnfs.SpendDetail), expectedErr error) {

	t.Helper()

	// Arrange a valid nonzero-index timeout spend, apply the caller's
	// mutation, and wire it into a side-effect-tracking resolver.
	htlcOutpoint := wire.OutPoint{Index: 3}
	timeoutTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: htlcOutpoint,
			Witness:          wire.TxWitness{{0x01}},
		}},
		TxOut: []*wire.TxOut{cloneTxOut(testSignDesc.Output)},
	}
	resolution := lnwallet.OutgoingHtlcResolution{
		ClaimOutpoint:   htlcOutpoint,
		SignedTimeoutTx: timeoutTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		SweepSignDesc: testSignDesc,
	}
	spendingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{},
			{
				PreviousOutPoint: htlcOutpoint,
				Witness:          wire.TxWitness{{0x01}},
			},
		},
		TxOut: []*wire.TxOut{
			{}, cloneTxOut(testSignDesc.Output),
		},
	}
	spend := newSpendDetail(htlcOutpoint, spendingTx, 1)
	prepare(&resolution, spend)

	ctx := newHtlcResolverTestContext(t, func(htlc channeldb.HTLC,
		cfg ResolverConfig) ContractResolver {

		return newTimeoutResolver(resolution, 0, htlc, 0, cfg)
	})
	var reports []*channeldb.ResolverReport
	ctx.checkpoint = func(_ ContractResolver,
		got ...*channeldb.ResolverReport) error {

		reports = got
		return nil
	}
	ctx.notifier.SpendChan <- spend

	resolver, ok := ctx.resolver.(*htlcTimeoutResolver)
	require.True(t, ok)

	// Act by resolving the live timeout spend through output matching and
	// terminal classification.
	err := resolver.resolveTimeoutTx()

	// Assert malformed state stops before resolution, while a formed
	// non-match checkpoints the actual spend as a terminal timeout.
	if expectedErr != nil {
		require.ErrorIs(t, err, expectedErr)
		require.False(t, resolver.IsResolved())
		require.Empty(t, reports)
		require.Empty(t, ctx.resolutionChan)
	} else {
		require.NoError(t, err)
		require.True(t, resolver.IsResolved())
		require.Len(t, reports, 1)
		require.Equal(t, htlcOutpoint, reports[0].OutPoint)
		require.Equal(t, spendingTx.TxHash(), *reports[0].SpendTxID)
		require.Equal(t, channeldb.ResolverOutcomeTimeout,
			reports[0].ResolverOutcome)
		require.NotNil(t, (<-ctx.resolutionChan).Failure)
	}

	// Assert both outcomes avoid phantom sweep and unrelated persistence or
	// notification side effects.
	sweeper, ok := resolver.Sweeper.(*mockSweeper)
	require.True(t, ok)
	require.Empty(t, sweeper.sweptInputs)
	require.False(t, ctx.finalHtlcOutcomeStored)
	require.Empty(t, ctx.htlcNotifier.finalHtlcEvents)
}

// TestHtlcTimeoutCheckpointsForeignOutput tests that a fully formed non-match
// is checkpointed as a terminal timeout without a phantom sweep.
func TestHtlcTimeoutCheckpointsForeignOutput(t *testing.T) {
	// Arrange a foreign spend by removing the committed output and optional
	// notifier provenance that resolution must normalize.
	// Act by delegating the live resolution to testHtlcTimeoutOutputMatch.
	// Assert there that the actual transaction is checkpointed.
	// No second-level sweep may be submitted for the terminal timeout.
	testHtlcTimeoutOutputMatch(t, func(_ *lnwallet.OutgoingHtlcResolution,
		spend *chainntnfs.SpendDetail) {

		spend.SpendingTx.TxOut = spend.SpendingTx.TxOut[:1]
		spend.SpentOutPoint = nil
		spend.SpenderTxHash = nil
	}, nil)
}

// TestHtlcTimeoutRejectsMalformedOutput tests that malformed spend or resolver
// data returns before producing terminal side effects.
func TestHtlcTimeoutRejectsMalformedOutput(t *testing.T) {
	// Arrange table mutations for malformed resolver and transaction
	// outputs, together with the sentinel each shape must preserve.
	testCases := []struct {
		name    string
		prepare func(*lnwallet.OutgoingHtlcResolution,
			*chainntnfs.SpendDetail)
		expectedErr error
	}{
		{
			name: "missing expected output",
			prepare: func(
				resolution *lnwallet.OutgoingHtlcResolution,
				_ *chainntnfs.SpendDetail) {

				resolution.SweepSignDesc.Output = nil
			},
			expectedErr: errInvalidSecondLevelOutput,
		},
		{
			name: "nil indexed output",
			prepare: func(_ *lnwallet.OutgoingHtlcResolution,
				spend *chainntnfs.SpendDetail) {

				spend.SpendingTx.TxOut[1] = nil
			},
			expectedErr: errInvalidSpendDetails,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Act by applying the case mutation in the shared live
			// timeout harness.
			// Assert there that the expected error precedes all
			// checkpoint, outcome, notification, and sweep effects.
			testHtlcTimeoutOutputMatch(
				t, testCase.prepare, testCase.expectedErr,
			)
		})
	}
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
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: commitOutpoint,
		}},
		TxOut: []*wire.TxOut{{}},
	}

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
	checkpoints []checkpoint, registrations ...chan wire.OutPoint) {

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
			if len(registrations) != 0 {
				r.Notifier = &recordingSpendNotifier{
					ChainNotifier: r.Notifier,
					registrations: registrations[0],
				}
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
