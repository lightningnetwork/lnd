package contractcourt

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var testHtlcAmt = lnwire.MilliSatoshi(200000)

type htlcResolverTestContext struct {
	resolver ContractResolver

	checkpoint func(_ ContractResolver,
		_ ...*channeldb.ResolverReport) error

	notifier *mock.ChainNotifier

	// htlcNotifier records final HTLC events for assertions.
	htlcNotifier *mockHTLCNotifier

	resolverResultChan chan resolveResult
	resolutionChan     chan ResolutionMsg

	finalHtlcOutcomeStored bool

	// finalHtlcSettled records the last stored final HTLC outcome.
	finalHtlcSettled bool

	t *testing.T
}

func newHtlcResolverTestContextFromReader(t *testing.T,
	newResolver func(htlc channeldb.HTLC,
		cfg ResolverConfig) ContractResolver) *htlcResolverTestContext {

	ctx := newHtlcResolverTestContext(t, newResolver)

	return ctx
}

func newHtlcResolverTestContext(t *testing.T,
	newResolver func(htlc channeldb.HTLC,
		cfg ResolverConfig) ContractResolver) *htlcResolverTestContext {

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		ConfChan:  make(chan *chainntnfs.TxConfirmation, 1),
	}
	htlcNotifier := &mockHTLCNotifier{}

	testCtx := &htlcResolverTestContext{
		checkpoint:     nil,
		notifier:       notifier,
		htlcNotifier:   htlcNotifier,
		resolutionChan: make(chan ResolutionMsg, 1),
		t:              t,
	}

	witnessBeacon := newMockWitnessBeacon()
	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:   notifier,
			PreimageDB: witnessBeacon,
			PublishTx: func(_ *wire.MsgTx, _ string) error {
				return nil
			},
			Sweeper: newMockSweeper(),
			IncubateOutputs: func(wire.OutPoint,
				fn.Option[lnwallet.OutgoingHtlcResolution],
				fn.Option[lnwallet.IncomingHtlcResolution],
				uint32, fn.Option[int32],
				...IncubateOption) error {

				return nil
			},
			DeliverResolutionMsg: func(msgs ...ResolutionMsg) error {
				if len(msgs) != 1 {
					return fmt.Errorf("expected 1 "+
						"resolution msg, instead got %v",
						len(msgs))
				}

				testCtx.resolutionChan <- msgs[0]
				return nil
			},
			PutFinalHtlcOutcome: func(chanId lnwire.ShortChannelID,
				htlcId uint64, settled bool) error {

				testCtx.finalHtlcOutcomeStored = true
				testCtx.finalHtlcSettled = settled

				return nil
			},
			HtlcNotifier: htlcNotifier,
			Budget:       *DefaultBudgetConfig(),
			QueryIncomingCircuit: func(
				circuit models.CircuitKey) *models.CircuitKey {

				return nil
			},
		},
		PutResolverReport: func(_ kvdb.RwTx,
			report *channeldb.ResolverReport) error {

			return nil
		},
	}
	// Since we want to replace this checkpoint method later in the test,
	// we wrap the call to it in a closure. The linter will complain about
	// this so set nolint directive.
	checkpointFunc := func(c ContractResolver, // nolint
		r ...*channeldb.ResolverReport) error {

		return testCtx.checkpoint(c, r...)
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint:              checkpointFunc,
	}

	htlc := channeldb.HTLC{
		RHash:     testResHash,
		OnionBlob: lnmock.MockOnion(),
		Amt:       testHtlcAmt,
	}

	testCtx.resolver = newResolver(htlc, cfg)

	return testCtx
}

func (i *htlcResolverTestContext) resolve() {
	// Start resolver.
	i.resolverResultChan = make(chan resolveResult, 1)

	go func() {
		err := i.resolver.Launch()
		require.NoError(i.t, err)

		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

func (i *htlcResolverTestContext) waitForResult() {
	i.t.Helper()

	result := <-i.resolverResultChan
	if result.err != nil {
		i.t.Fatal(result.err)
	}

	if result.nextResolver != nil {
		i.t.Fatal("expected no next resolver")
	}
}

// TestHtlcSuccessSingleStage tests successful sweep of a single stage htlc
// claim.
func TestHtlcSuccessSingleStage(t *testing.T) {
	htlcOutpoint := wire.OutPoint{Index: 3}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}

	// singleStageResolution is a resolution for a htlc on the remote
	// party's commitment.
	singleStageResolution := lnwallet.IncomingHtlcResolution{
		SweepSignDesc: testSignDesc,
		ClaimOutpoint: htlcOutpoint,
	}

	sweepTxid := sweepTx.TxHash()
	claim := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &sweepTxid,
	}

	checkpoints := []checkpoint{
		{
			// We send a confirmation for our sweep tx to indicate
			// that our sweep succeeded.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				// The resolver will offer the input to the
				// sweeper.
				details := &chainntnfs.SpendDetail{
					SpendingTx:    sweepTx,
					SpentOutPoint: &htlcOutpoint,
					SpenderTxHash: &sweepTxid,
				}
				ctx.notifier.SpendChan <- details

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// report.
			resolved: true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
			finalHtlcStored: true,
		},
	}

	testHtlcSuccess(
		t, singleStageResolution, checkpoints,
	)
}

// TestHtlcSuccessSecondStageResolution tests successful sweep of a second
// stage htlc claim, going through the Nursery.
func TestHtlcSuccessSecondStageResolution(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	htlcOutpoint := wire.OutPoint{Index: 3}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}
	sweepHash := sweepTx.TxHash()

	// twoStageResolution is a resolution for htlc on our own commitment
	// which is spent from the signed success tx.
	twoStageResolution := lnwallet.IncomingHtlcResolution{
		Preimage: [32]byte{},
		SignedSuccessTx: &wire.MsgTx{
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
		},
		ClaimOutpoint: htlcOutpoint,
		SweepSignDesc: testSignDesc,
	}

	successTx := twoStageResolution.SignedSuccessTx.TxHash()
	firstStage := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &successTx,
	}

	secondStage := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &sweepHash,
	}

	checkpoints := []checkpoint{
		{
			// The resolver will send the output to the Nursery.
			incubating: true,
		},
		{
			// It will then wait for the Nursery to spend the
			// output. We send a spend notification for our output
			// to resolve our htlc.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    sweepTx,
					SpentOutPoint: &htlcOutpoint,
					SpenderTxHash: &sweepHash,
				}

				return nil
			},
			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				secondStage,
				firstStage,
			},
			finalHtlcStored: true,
		},
	}

	testHtlcSuccess(
		t, twoStageResolution, checkpoints,
	)
}

// TestHtlcSuccessSecondStageResolutionSweeper test that a resolver with
// non-nil SignDetails will offer the second-level transaction to the sweeper
// for re-signing.
//
//nolint:ll
func TestHtlcSuccessSecondStageResolutionSweeper(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}

	successTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			cloneTxOut(testSignDesc.Output),
		},
	}
	successHash := successTx.TxHash()
	htlcOutpoint := wire.OutPoint{
		Hash:  successHash,
		Index: 0,
	}

	sweepTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{

			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  successHash,
					Index: 0,
				},
			},
		},
		TxOut: []*wire.TxOut{{}},
	}
	sweepHash := sweepTx.TxHash()

	// twoStageResolution is a resolution for htlc on our own commitment
	// which is spent from the signed success tx.
	twoStageResolution := lnwallet.IncomingHtlcResolution{
		Preimage:        [32]byte{},
		CsvDelay:        4,
		SignedSuccessTx: successTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		ClaimOutpoint: htlcOutpoint,
		SweepSignDesc: testSignDesc,
	}

	firstStage := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &successHash,
	}

	secondStage := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &sweepHash,
	}

	checkpoints := []checkpoint{
		{
			// The HTLC output on the commitment should be offered
			// to the sweeper. We'll notify that it gets spent.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				_ bool) error {

				resolver := ctx.resolver.(*htlcSuccessResolver)

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
				if op != commitOutpoint {
					return fmt.Errorf("outpoint %v swept, "+
						"expected %v", op,
						commitOutpoint)
				}

				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:        successTx,
					SpenderTxHash:     &successHash,
					SpenderInputIndex: 0,
					SpendingHeight:    10,
					SpentOutPoint:     &commitOutpoint,
				}
				return nil
			},
			// incubating=true is used to signal that the
			// second-level transaction was confirmed.
			incubating: true,
		},
		{
			// The resolver will wait for the second-level's CSV
			// lock to expire.
			preCheckpoint: func(ctx *htlcResolverTestContext,
				resumed bool) error {

				// If we are resuming from a checkpoint, we
				// expect the resolver to re-subscribe to a
				// spend, hence we must resend it.
				if resumed {
					ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
						SpendingTx:        successTx,
						SpenderTxHash:     &successHash,
						SpenderInputIndex: 0,
						SpendingHeight:    10,
						SpentOutPoint:     &commitOutpoint,
					}
				}

				// We expect it to sweep the second-level
				// transaction we notfied about above.
				resolver := ctx.resolver.(*htlcSuccessResolver)

				// Mock `waitForSpend` to return the commit
				// spend.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:        successTx,
					SpenderTxHash:     &successHash,
					SpenderInputIndex: 0,
					SpendingHeight:    10,
					SpentOutPoint:     &commitOutpoint,
				}

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
					Hash:  successHash,
					Index: 0,
				}
				if op != exp {
					return fmt.Errorf("swept outpoint %v, expected %v",
						op, exp)
				}

				// Notify about the spend, which should resolve
				// the resolver.
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:     sweepTx,
					SpenderTxHash:  &sweepHash,
					SpendingHeight: 14,
					SpentOutPoint:  &op,
				}

				return nil
			},

			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				secondStage,
				firstStage,
			},
			finalHtlcStored: true,
		},
	}

	testHtlcSuccess(t, twoStageResolution, checkpoints)
}

// TestHtlcSuccessResolverRejectsForeignSpend asserts that a spend which does
// not create the expected second-level success output fails the resolver
// instead of handing a phantom output to the sweeper.
func TestHtlcSuccessResolverRejectsForeignSpend(t *testing.T) {
	defer timeout()()

	// Build a success resolution whose expected second-level output can be
	// distinguished from the foreign spender below.
	commitOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x01},
		Index: 2,
	}
	resolution := newTestAnchorSuccessResolution(commitOutpoint)
	foreignTx, foreignHash := newForeignSuccessSpend(commitOutpoint)
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &foreignHash,
	}
	ctx := newTestHtlcSuccessContext(t, resolution, false)

	checkpointChan := make(chan struct{}, 1)
	ctx.checkpoint = func(resolver ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		successResolver, ok := resolver.(*htlcSuccessResolver)
		require.True(t, ok)

		require.True(t, successResolver.IsResolved())
		require.False(t, successResolver.outputIncubating)
		require.True(t, ctx.finalHtlcOutcomeStored)
		require.False(t, ctx.finalHtlcSettled)
		require.Equal(t, []*channeldb.ResolverReport{
			expectedReport,
		}, reports)

		checkpointChan <- struct{}{}

		return nil
	}

	// Start the resolver and wait for it to offer the commitment HTLC as a
	// success input to the sweeper.
	ctx.resolve()

	resolver := successResolverFromContext(t, ctx)
	sweeper := mockSweeperFromResolver(t, resolver)
	select {
	case inp := <-sweeper.sweptInputs:
		require.Equal(t, commitOutpoint, inp.OutPoint())
	case <-time.After(time.Second):
		t.Fatal("expected first-stage input to be swept")
	}

	// Notify that a different transaction spent the commitment HTLC without
	// creating the expected success output.
	ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
		SpendingTx:        foreignTx,
		SpenderTxHash:     &foreignHash,
		SpenderInputIndex: 0,
		SpendingHeight:    10,
		SpentOutPoint:     &commitOutpoint,
	}

	select {
	case <-checkpointChan:
	case <-time.After(time.Second):
		t.Fatal("expected foreign spend checkpoint")
	}
	ctx.waitForResult()

	// The resolver should checkpoint the foreign spend as a final failure.
	// It should not offer a derived second-level output to the sweeper.
	assertNoSweptInput(t, sweeper)
	assertFinalHtlcFailed(t, ctx)
}

// TestHtlcSuccessResolverRejectsForeignSpendOnRestart asserts that a restarted
// resolver with outputIncubating already set still rejects a foreign
// first-stage spender instead of sweeping a derived phantom output.
func TestHtlcSuccessResolverRejectsForeignSpendOnRestart(t *testing.T) {
	defer timeout()()

	// Start from an incubating resolver to exercise the restart
	// fast-forward path that immediately waits for the commitment HTLC
	// spend.
	commitOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x02},
		Index: 3,
	}
	resolution := newTestAnchorSuccessResolution(commitOutpoint)
	foreignTx, foreignHash := newForeignSuccessSpend(commitOutpoint)
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       &foreignHash,
	}
	ctx := newTestHtlcSuccessContext(t, resolution, true)

	checkpointChan := make(chan struct{}, 1)
	ctx.checkpoint = func(resolver ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		successResolver, ok := resolver.(*htlcSuccessResolver)
		require.True(t, ok)

		require.True(t, successResolver.IsResolved())
		require.False(t, successResolver.outputIncubating)
		require.True(t, ctx.finalHtlcOutcomeStored)
		require.False(t, ctx.finalHtlcSettled)
		require.Equal(t, []*channeldb.ResolverReport{
			expectedReport,
		}, reports)

		checkpointChan <- struct{}{}

		return nil
	}

	// Notify a foreign spender twice. Launch validates the historical
	// spend but leaves terminal checkpointing to Resolve, which waits for
	// the same spend again in this mock setup.
	ctx.resolve()
	foreignSpend := &chainntnfs.SpendDetail{
		SpendingTx:        foreignTx,
		SpenderTxHash:     &foreignHash,
		SpenderInputIndex: 0,
		SpendingHeight:    10,
		SpentOutPoint:     &commitOutpoint,
	}
	go func() {
		ctx.notifier.SpendChan <- foreignSpend
		ctx.notifier.SpendChan <- foreignSpend
	}()

	select {
	case <-checkpointChan:
	case <-time.After(time.Second):
		t.Fatal("expected foreign spend checkpoint")
	}
	ctx.waitForResult()

	// The restart path should checkpoint the same final failed outcome as
	// the non-restart path.
	resolver := successResolverFromContext(t, ctx)
	assertNoSweptInput(t, mockSweeperFromResolver(t, resolver))
	assertFinalHtlcFailed(t, ctx)
}

// TestHtlcSuccessResolverForeignSpendCheckpointError asserts that the final
// HTLC event is emitted only after the foreign-spend checkpoint succeeds.
func TestHtlcSuccessResolverForeignSpendCheckpointError(t *testing.T) {
	defer timeout()()

	// Return a checkpoint error after the resolver prepares its terminal
	// foreign-spend state. This simulates the DB rejecting the checkpoint.
	commitOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x03},
		Index: 4,
	}
	resolution := newTestAnchorSuccessResolution(commitOutpoint)
	foreignTx, foreignHash := newForeignSuccessSpend(commitOutpoint)
	errCheckpoint := errors.New("checkpoint failed")
	ctx := newTestHtlcSuccessContext(t, resolution, false)

	checkpointChan := make(chan struct{}, 1)
	ctx.checkpoint = func(resolver ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		successResolver, ok := resolver.(*htlcSuccessResolver)
		require.True(t, ok)

		require.True(t, successResolver.IsResolved())
		require.Len(t, reports, 1)

		checkpointChan <- struct{}{}

		return errCheckpoint
	}

	// Drive the resolver until it offers the first-stage success input,
	// then notify a foreign spender that triggers the failing checkpoint
	// path.
	ctx.resolve()

	resolver := successResolverFromContext(t, ctx)
	sweeper := mockSweeperFromResolver(t, resolver)
	select {
	case inp := <-sweeper.sweptInputs:
		require.Equal(t, commitOutpoint, inp.OutPoint())
	case <-time.After(time.Second):
		t.Fatal("expected first-stage input to be swept")
	}

	ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
		SpendingTx:        foreignTx,
		SpenderTxHash:     &foreignHash,
		SpenderInputIndex: 0,
		SpendingHeight:    10,
		SpentOutPoint:     &commitOutpoint,
	}

	select {
	case <-checkpointChan:
	case <-time.After(time.Second):
		t.Fatal("expected foreign spend checkpoint")
	}

	result := <-ctx.resolverResultChan
	require.ErrorIs(t, result.err, errCheckpoint)
	require.Nil(t, result.nextResolver)
	require.True(t, resolver.IsResolved())
	require.Empty(t, ctx.htlcNotifier.finalHtlcEvents)
}

// newTestHtlcSuccessContext creates a success resolver test context with the
// supplied incoming HTLC resolution and incubation state.
func newTestHtlcSuccessContext(t *testing.T,
	resolution lnwallet.IncomingHtlcResolution,
	outputIncubating bool) *htlcResolverTestContext {

	return newHtlcResolverTestContext(
		t, func(htlc channeldb.HTLC,
			cfg ResolverConfig) ContractResolver {

			r := newSuccessResolver(
				resolution, 0, htlc, 0, cfg,
			)
			r.outputIncubating = outputIncubating

			return r
		},
	)
}

// successResolverFromContext returns the success resolver installed in a test
// context.
func successResolverFromContext(t *testing.T,
	ctx *htlcResolverTestContext) *htlcSuccessResolver {

	t.Helper()

	resolver, ok := ctx.resolver.(*htlcSuccessResolver)
	require.True(t, ok)

	return resolver
}

// mockSweeperFromResolver returns the mock sweeper used by a success resolver.
func mockSweeperFromResolver(t *testing.T,
	resolver *htlcSuccessResolver) *mockSweeper {

	t.Helper()

	sweeper, ok := resolver.Sweeper.(*mockSweeper)
	require.True(t, ok)

	return sweeper
}

// newTestAnchorSuccessResolution returns an anchor-channel success resolution
// whose success transaction creates the expected second-level output.
func newTestAnchorSuccessResolution(
	commitOutpoint wire.OutPoint) lnwallet.IncomingHtlcResolution {

	successTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			cloneTxOut(testSignDesc.Output),
		},
	}
	successHash := successTx.TxHash()

	return lnwallet.IncomingHtlcResolution{
		Preimage:        [32]byte{},
		CsvDelay:        4,
		SignedSuccessTx: successTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		ClaimOutpoint: wire.OutPoint{
			Hash:  successHash,
			Index: 0,
		},
		SweepSignDesc: testSignDesc,
	}
}

// newForeignSuccessSpend returns a transaction that spends the commitment HTLC
// without creating the expected second-level success output.
func newForeignSuccessSpend(
	commitOutpoint wire.OutPoint) (*wire.MsgTx, chainhash.Hash) {

	foreignTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: commitOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    testSignDesc.Output.Value,
				PkScript: []byte{0x51},
			},
		},
	}

	return foreignTx, foreignTx.TxHash()
}

// cloneTxOut returns a copy of a transaction output.
func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	pkScript := append([]byte(nil), txOut.PkScript...)
	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: pkScript,
	}
}

// assertNoSweptInput asserts that the sweeper was not offered another input.
func assertNoSweptInput(t *testing.T, sweeper *mockSweeper) {
	t.Helper()

	select {
	case inp := <-sweeper.sweptInputs:
		t.Fatalf("unexpected swept input: %v", inp.OutPoint())
	default:
	}
}

// assertFinalHtlcFailed asserts that the final HTLC outcome was failed.
func assertFinalHtlcFailed(t *testing.T, ctx *htlcResolverTestContext) {
	t.Helper()

	require.True(t, ctx.finalHtlcOutcomeStored)
	require.False(t, ctx.finalHtlcSettled)
}

// checkpoint holds expected data we expect the resolver to checkpoint itself
// to the DB next.
type checkpoint struct {
	// preCheckpoint is a method that will be called before we reach the
	// checkpoint, to carry out any needed operations to drive the resolver
	// in this stage.
	preCheckpoint func(*htlcResolverTestContext, bool) error

	// data we expect the resolver to be checkpointed with next.
	incubating      bool
	resolved        bool
	reports         []*channeldb.ResolverReport
	finalHtlcStored bool
}

// testHtlcSuccess tests resolution of a success resolver. It takes a a list of
// checkpoints that it expects the resolver to go through. And will run the
// resolver all the way through these checkpoints, and also attempt to resume
// the resolver from every checkpoint.
func testHtlcSuccess(t *testing.T, resolution lnwallet.IncomingHtlcResolution,
	checkpoints []checkpoint) {

	defer timeout()()

	// We first run the resolver from start to finish, ensuring it gets
	// checkpointed at every expected stage. We store the checkpointed data
	// for the next portion of the test.
	ctx := newHtlcResolverTestContext(t,
		func(htlc channeldb.HTLC, cfg ResolverConfig) ContractResolver {
			r := &htlcSuccessResolver{
				contractResolverKit: *newContractResolverKit(cfg),
				htlc:                htlc,
				htlcResolution:      resolution,
			}
			r.initLogger("htlcSuccessResolver")

			return r
		},
	)

	checkpointedState := runFromCheckpoint(t, ctx, checkpoints)

	// Now, from every checkpoint created, we re-create the resolver, and
	// run the test from that checkpoint.
	for i := range checkpointedState {
		cp := bytes.NewReader(checkpointedState[i])
		ctx := newHtlcResolverTestContext(t,
			func(htlc channeldb.HTLC, cfg ResolverConfig) ContractResolver {
				resolver, err := newSuccessResolverFromReader(cp, cfg)
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

// runFromCheckpoint executes the Resolve method on the success resolver, and
// asserts that it checkpoints itself according to the expected checkpoints.
func runFromCheckpoint(t *testing.T, ctx *htlcResolverTestContext,
	expectedCheckpoints []checkpoint) [][]byte {

	defer timeout()()

	var checkpointedState [][]byte

	// Replace our checkpoint method with one which we'll use to assert the
	// checkpointed state and reports are equal to what we expect.
	nextCheckpoint := 0
	checkpointChan := make(chan struct{})
	ctx.checkpoint = func(resolver ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		if nextCheckpoint >= len(expectedCheckpoints) {
			t.Fatal("did not expect more checkpoints")
		}

		var resolved, incubating bool
		if h, ok := resolver.(*htlcSuccessResolver); ok {
			resolved = h.resolved.Load()
			incubating = h.outputIncubating
		}
		if h, ok := resolver.(*htlcTimeoutResolver); ok {
			resolved = h.resolved.Load()
			incubating = h.outputIncubating
		}

		cp := expectedCheckpoints[nextCheckpoint]

		if resolved != cp.resolved {
			t.Fatalf("expected checkpoint to be resolve=%v, had %v",
				cp.resolved, resolved)
		}

		if !reflect.DeepEqual(incubating, cp.incubating) {
			t.Fatalf("expected checkpoint to be have "+
				"incubating=%v, had %v", cp.incubating,
				incubating)
		}

		// Check we go the expected reports.
		if len(reports) != len(cp.reports) {
			t.Fatalf("unexpected number of reports. Expected %v "+
				"got %v", len(cp.reports), len(reports))
		}

		for i, report := range reports {
			if !reflect.DeepEqual(report, cp.reports[i]) {
				t.Fatalf("expected: %v, got: %v",
					spew.Sdump(cp.reports[i]),
					spew.Sdump(report))
			}
		}

		// Check that the final htlc outcome is stored.
		if cp.finalHtlcStored != ctx.finalHtlcOutcomeStored {
			t.Fatal("final htlc store expectation failed")
		}

		// Finally encode the resolver, and store it for later use.
		b := bytes.Buffer{}
		if err := resolver.Encode(&b); err != nil {
			t.Fatal(err)
		}

		checkpointedState = append(checkpointedState, b.Bytes())
		nextCheckpoint++
		select {
		case checkpointChan <- struct{}{}:
		case <-time.After(1 * time.Second):
			t.Fatal("checkpoint timeout")
		}

		return nil
	}

	// Start the htlc success resolver.
	ctx.resolve()

	// Go through our list of expected checkpoints, so we can run the
	// preCheckpoint logic if needed.
	resumed := true
	for i, cp := range expectedCheckpoints {
		t.Logf("Running checkpoint %d", i)

		if cp.preCheckpoint != nil {
			if err := cp.preCheckpoint(ctx, resumed); err != nil {
				t.Fatalf("failure at stage %d: %v", i, err)
			}
		}
		resumed = false

		// Wait for the resolver to have checkpointed its state.
		select {
		case <-checkpointChan:
		case <-time.After(1 * time.Second):
			t.Fatalf("resolver did not checkpoint at stage %d", i)
		}
	}

	// Wait for the resolver to fully complete.
	ctx.waitForResult()

	return checkpointedState
}
