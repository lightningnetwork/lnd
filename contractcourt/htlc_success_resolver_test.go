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
	"github.com/btcsuite/btcd/txscript/v2"
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

	notifier           *mock.ChainNotifier
	htlcNotifier       *mockHTLCNotifier
	resolverResultChan chan resolveResult
	resolutionChan     chan ResolutionMsg

	finalHtlcOutcomeStored bool
	finalHtlcSettled       bool

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

// TestHtlcSuccessSingleStage tests classification of a direct HTLC spend.
func TestHtlcSuccessSingleStage(t *testing.T) {
	taprootPkScript := append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32}, make([]byte, 32)...,
	)
	wrongPreimage := make([]byte, 32)
	wrongPreimage[0] = 1
	resolverType := channeldb.ResolverTypeIncomingHtlc
	testCases := []struct {
		name    string
		witness wire.TxWitness
		success bool
		taproot bool
		index   uint32
	}{
		{
			name: "success", success: true, index: 1,
			witness: wire.TxWitness{
				dummyBytes, testResPreimage[:], dummyBytes,
			},
		},
		{
			name: "timeout",
			witness: wire.TxWitness{
				nil, dummyBytes, dummyBytes, nil, dummyBytes,
			},
		},
		{
			name: "wrong preimage",
			witness: wire.TxWitness{
				dummyBytes, wrongPreimage, dummyBytes,
			},
		},
		{
			name: "taproot success", success: true, taproot: true,
			witness: wire.TxWitness{
				dummyBytes, testResPreimage[:], dummyBytes,
				dummyBytes,
			},
		},
		{
			name: "taproot timeout", taproot: true,
			witness: wire.TxWitness{
				dummyBytes, dummyBytes, dummyBytes, dummyBytes,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			htlcOutpoint := wire.OutPoint{Index: 3}
			inputs := []*wire.TxIn{{
				PreviousOutPoint: htlcOutpoint,
				Witness:          tc.witness,
			}}
			if tc.index != 0 {
				inputs = append([]*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Index: 4,
					},
				}}, inputs...)
			}
			sweepTx := &wire.MsgTx{
				TxIn:  inputs,
				TxOut: []*wire.TxOut{{}},
			}
			sweepTxid := sweepTx.TxHash()

			signDesc := testSignDesc
			if tc.taproot {
				signDesc.Output = cloneTxOut(
					testSignDesc.Output,
				)
				signDesc.Output.PkScript = taprootPkScript
			}
			resolution := lnwallet.IncomingHtlcResolution{
				Preimage:      testResPreimage,
				SweepSignDesc: signDesc,
				ClaimOutpoint: htlcOutpoint,
			}
			outcome := channeldb.ResolverOutcomeTimeout
			amount := testHtlcAmt.ToSatoshis()
			if tc.success {
				outcome = channeldb.ResolverOutcomeClaimed
				amount = btcutil.Amount(
					testSignDesc.Output.Value,
				)
			}
			report := &channeldb.ResolverReport{
				OutPoint:        htlcOutpoint,
				Amount:          amount,
				ResolverType:    resolverType,
				ResolverOutcome: outcome,
				SpendTxID:       &sweepTxid,
			}
			checkpoints := []checkpoint{{
				preCheckpoint: func(
					ctx *htlcResolverTestContext,
					_ bool) error {

					spend := newSpendDetail(
						htlcOutpoint, sweepTx, tc.index,
					)
					ctx.notifier.SpendChan <- spend

					return nil
				},
				resolved: true,
				reports: []*channeldb.ResolverReport{
					report,
				},
				finalHtlcStored:  true,
				finalHtlcSettled: tc.success,
			}}
			testHtlcSuccess(t, resolution, checkpoints)
		})
	}
}

func TestHtlcSuccessRemoteSpendValidation(t *testing.T) {
	resolver := &htlcSuccessResolver{
		htlcResolution: lnwallet.IncomingHtlcResolution{
			ClaimOutpoint: wire.OutPoint{Index: 1},
			SweepSignDesc: testSignDesc,
		},
	}

	matches, err := resolver.isRemoteCommitSuccessSpend(nil)
	require.ErrorContains(t, err, "missing spend detail")
	require.False(t, matches)
}

// TestHtlcSuccessSecondStageResolution tests successful sweep of a second
// stage htlc claim, going through the Nursery.
func TestHtlcSuccessSecondStageResolution(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	successTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: commitOutpoint,
		}},
		TxOut: []*wire.TxOut{{
			Value:    111,
			PkScript: []byte{0xaa, 0xaa},
		}},
	}
	successHash := successTx.TxHash()
	htlcOutpoint := wire.OutPoint{Hash: successHash}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{{}},
	}
	sweepHash := sweepTx.TxHash()

	// twoStageResolution is a resolution for htlc on our own commitment
	// which is spent from the signed success tx.
	twoStageResolution := lnwallet.IncomingHtlcResolution{
		Preimage:        [32]byte{},
		SignedSuccessTx: successTx,
		ClaimOutpoint:   htlcOutpoint,
		SweepSignDesc:   testSignDesc,
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
			finalHtlcStored:  true,
			finalHtlcSettled: true,
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
	twoStageResolution := newSuccessTestResolution(commitOutpoint)
	twoStageResolution.CsvDelay = 4
	successTx := twoStageResolution.SignedSuccessTx

	reSignedSuccessTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{Index: 10},
			},
			successTx.TxIn[0],
			{
				PreviousOutPoint: wire.OutPoint{Index: 11},
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    111,
				PkScript: []byte{0xaa, 0xaa},
			},
			cloneTxOut(successTx.TxOut[0]),
		},
	}
	reSignedHash := reSignedSuccessTx.TxHash()
	secondLevelOutpoint := wire.OutPoint{
		Hash:  reSignedHash,
		Index: 1,
	}

	sweepTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{PreviousOutPoint: secondLevelOutpoint}},
		TxOut: []*wire.TxOut{{}},
	}
	sweepHash := sweepTx.TxHash()

	firstStage := &channeldb.ResolverReport{
		OutPoint:        commitOutpoint,
		Amount:          testHtlcAmt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &reSignedHash,
	}

	secondStage := &channeldb.ResolverReport{
		OutPoint:        secondLevelOutpoint,
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

				requireSweptOutpoint(t, ctx.resolver, commitOutpoint)
				ctx.notifier.SpendChan <- newSpendDetail(
					commitOutpoint, reSignedSuccessTx, 1,
				)
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
					ctx.notifier.SpendChan <- newSpendDetail(
						commitOutpoint, reSignedSuccessTx, 1,
					)
				}

				// We expect it to sweep the second-level
				// transaction we notfied about above.
				// Mock `waitForSpend` to return the commit
				// spend.
				ctx.notifier.SpendChan <- newSpendDetail(
					commitOutpoint, reSignedSuccessTx, 1,
				)
				requireSweptOutpoint(
					t, ctx.resolver, secondLevelOutpoint,
				)

				// Notify about the spend, which should resolve
				// the resolver.
				ctx.notifier.SpendChan <- newSpendDetail(
					secondLevelOutpoint, sweepTx, 0,
				)

				return nil
			},

			incubating: true,
			resolved:   true,
			reports: []*channeldb.ResolverReport{
				secondStage,
				firstStage,
			},
			finalHtlcStored:  true,
			finalHtlcSettled: true,
		},
	}

	testHtlcSuccess(t, twoStageResolution, checkpoints)
}

func TestHtlcSuccessMatchSecondLevelOutput(t *testing.T) {
	claim := wire.OutPoint{Index: 2}
	newMatch := func() (*htlcSuccessResolver, *chainntnfs.SpendDetail) {
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: wire.OutPoint{Index: 1}},
				{PreviousOutPoint: claim},
			},
			TxOut: []*wire.TxOut{
				cloneTxOut(testSignDesc.Output),
				cloneTxOut(testSignDesc.Output),
			},
		}

		return &htlcSuccessResolver{
			htlcResolution: newSuccessTestResolution(claim),
		}, newSpendDetail(claim, tx, 1)
	}
	resolver, spend := newMatch()
	check := func(expected bool) {
		matches, err := resolver.matchSecondLevelOutput(spend)
		require.NoError(t, err)
		require.Equal(t, expected, matches)
	}
	check(true)
	spend.SpendingTx.TxOut[1].Value++
	spend = newSpendDetail(claim, spend.SpendingTx, 1)
	require.Equal(t, testSignDesc.Output, spend.SpendingTx.TxOut[0])
	check(false)
	resolver, spend = newMatch()
	spend.SpendingTx.TxOut = nil
	spend = newSpendDetail(claim, spend.SpendingTx, 1)
	check(false)
	for _, want := range []string{
		"missing spend detail", "missing spending tx",
		"missing spender txid", "missing spent outpoint",
		"unexpected outpoint", "spender input index",
		"spender input 0 is nil", "missing expected output",
		"output 0 is nil", "input", "does not match tx",
	} {
		resolver, spend := newMatch()
		switch want {
		case "missing spend detail":
			spend = nil
		case "missing spending tx":
			spend.SpendingTx = nil
		case "missing spender txid":
			spend.SpenderTxHash = nil
		case "missing spent outpoint":
			spend.SpentOutPoint = nil
		case "unexpected outpoint":
			spend.SpentOutPoint = &wire.OutPoint{}
		case "spender input index":
			spend.SpenderInputIndex = 2
		case "spender input 0 is nil":
			spend.SpendingTx.TxIn[0] = nil
		case "missing expected output":
			resolver.htlcResolution.SweepSignDesc.Output = nil
		case "output 0 is nil":
			spend.SpendingTx.TxOut[0] = nil
		case "input":
			spend.SpendingTx.TxIn[1].PreviousOutPoint.Index++
		case "does not match tx":
			spend.SpenderTxHash = &chainhash.Hash{}
		}
		matches, err := resolver.matchSecondLevelOutput(spend)
		require.ErrorContains(t, err, want)
		require.False(t, matches)
	}
}

func TestHtlcSuccessForeignSpendCheckpointError(t *testing.T) {
	t.Parallel()

	commitOutpoint := wire.OutPoint{Index: 4}
	resolution := newSuccessTestResolution(commitOutpoint)
	ctx := newHtlcResolverTestContext(t, func(htlc channeldb.HTLC,
		cfg ResolverConfig) ContractResolver {

		return newSuccessResolver(resolution, 0, htlc, 0, cfg)
	})
	resolver := requireSuccessResolver(t, ctx.resolver)
	resolver.outputIncubating = true
	resolver.currentReport.RecoveredBalance = 1
	previousReport := resolver.currentReport

	errCheckpoint := errors.New("checkpoint failed")
	checkpointCalls := 0
	ctx.checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		checkpointCalls++
		require.True(t, resolver.IsResolved())
		require.False(t, resolver.outputIncubating)
		require.Zero(t, resolver.currentReport.LimboBalance)
		require.Zero(t, resolver.currentReport.RecoveredBalance)
		require.Len(t, reports, 1)
		require.Equal(t, channeldb.ResolverOutcomeTimeout,
			reports[0].ResolverOutcome)
		if checkpointCalls == 1 {
			return errCheckpoint
		}

		return nil
	}

	foreignTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{PreviousOutPoint: commitOutpoint}},
		TxOut: []*wire.TxOut{{PkScript: []byte{0x51}}},
	}
	spend := newSpendDetail(commitOutpoint, foreignTx, 0)

	err := resolver.checkpointForeignSpend(spend)
	require.ErrorIs(t, err, errCheckpoint)
	require.False(t, resolver.IsResolved())
	require.True(t, resolver.outputIncubating)
	require.Equal(t, previousReport, resolver.currentReport)
	require.Empty(t, ctx.htlcNotifier.finalHtlcEvents)

	require.NoError(t, resolver.checkpointForeignSpend(spend))
	require.Equal(t, 2, checkpointCalls)
	require.True(t, resolver.IsResolved())
	require.False(t, resolver.outputIncubating)
	require.Zero(t, resolver.currentReport.LimboBalance)
	require.Zero(t, resolver.currentReport.RecoveredBalance)
	require.Equal(t, []channeldb.FinalHtlcInfo{{
		Settled: false,
	}}, ctx.htlcNotifier.finalHtlcEvents)
}

func TestHtlcSuccessForeignSpend(t *testing.T) {
	commitOutpoint := wire.OutPoint{Index: 2}
	resolution := newSuccessTestResolution(commitOutpoint)
	foreignTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{PreviousOutPoint: commitOutpoint}},
		TxOut: []*wire.TxOut{{
			Value:    testSignDesc.Output.Value,
			PkScript: []byte{0x51},
		}},
	}
	foreignSpend := newSpendDetail(commitOutpoint, foreignTx, 0)
	resolverType := channeldb.ResolverTypeIncomingHtlc
	resolverOutcome := channeldb.ResolverOutcomeTimeout
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			defer timeout()()
			ctx := newHtlcResolverTestContext(
				t, func(htlc channeldb.HTLC,
					cfg ResolverConfig) ContractResolver {

					resolver := newSuccessResolver(
						resolution, 0, htlc, 0, cfg,
					)
					if !restart {
						return resolver
					}

					resolver.outputIncubating = true
					var state bytes.Buffer
					err := resolver.Encode(&state)
					require.NoError(t, err)
					decode := newSuccessResolverFromReader
					restored, err := decode(&state, cfg)
					require.NoError(t, err)
					restored.Supplement(htlc)

					return restored
				},
			)

			var reports []*channeldb.ResolverReport
			ctx.checkpoint = func(_ ContractResolver,
				r ...*channeldb.ResolverReport) error {

				reports = r
				return nil
			}

			ctx.resolve()
			resolver := requireSuccessResolver(t, ctx.resolver)
			if !restart {
				requireSweptOutpoint(
					t, resolver, commitOutpoint,
				)
			}
			go func() {
				ctx.notifier.SpendChan <- foreignSpend
				if restart {
					ctx.notifier.SpendChan <- foreignSpend
				}
			}()
			ctx.waitForResult()

			require.True(t, resolver.IsResolved())
			require.False(t, resolver.outputIncubating)
			require.Zero(t, resolver.currentReport.LimboBalance)
			require.Zero(t, resolver.currentReport.RecoveredBalance)
			require.True(t, ctx.finalHtlcOutcomeStored)
			require.False(t, ctx.finalHtlcSettled)
			require.Equal(t, []*channeldb.ResolverReport{{
				OutPoint:        commitOutpoint,
				Amount:          testHtlcAmt.ToSatoshis(),
				ResolverType:    resolverType,
				ResolverOutcome: resolverOutcome,
				SpendTxID:       foreignSpend.SpenderTxHash,
			}}, reports)
			require.Equal(t, []channeldb.FinalHtlcInfo{{
				Settled: false,
			}}, ctx.htlcNotifier.finalHtlcEvents)

			sweeper := requireMockSweeper(t, resolver)
			select {
			case sweptInput := <-sweeper.sweptInputs:
				t.Fatalf("unexpected phantom sweep: %v",
					sweptInput.OutPoint())
			default:
			}
		})
	}
}

// cloneTxOut returns a copy of a transaction output.
func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	pkScript := append([]byte(nil), txOut.PkScript...)
	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: pkScript,
	}
}

func newSuccessTestResolution(
	commitOutpoint wire.OutPoint) lnwallet.IncomingHtlcResolution {

	successTx := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{PreviousOutPoint: commitOutpoint}},
		TxOut: []*wire.TxOut{cloneTxOut(testSignDesc.Output)},
	}

	return lnwallet.IncomingHtlcResolution{
		Preimage:        testResPreimage,
		SignedSuccessTx: successTx,
		SignDetails: &input.SignDetails{
			SignDesc: testSignDesc,
			PeerSig:  testSig,
		},
		ClaimOutpoint: wire.OutPoint{Hash: successTx.TxHash()},
		SweepSignDesc: testSignDesc,
	}
}

func newSpendDetail(spent wire.OutPoint, tx *wire.MsgTx,
	inputIndex uint32) *chainntnfs.SpendDetail {

	txid := tx.TxHash()
	return &chainntnfs.SpendDetail{
		SpendingTx:        tx,
		SpenderTxHash:     &txid,
		SpenderInputIndex: inputIndex,
		SpendingHeight:    10,
		SpentOutPoint:     &spent,
	}
}

func requireSuccessResolver(t *testing.T,
	resolver ContractResolver) *htlcSuccessResolver {

	t.Helper()
	successResolver, ok := resolver.(*htlcSuccessResolver)
	require.True(t, ok)

	return successResolver
}

func requireMockSweeper(t *testing.T,
	resolver *htlcSuccessResolver) *mockSweeper {

	t.Helper()
	sweeper, ok := resolver.Sweeper.(*mockSweeper)
	require.True(t, ok)

	return sweeper
}

func requireSweptOutpoint(t *testing.T, resolver ContractResolver,
	expected wire.OutPoint) {

	t.Helper()
	successResolver := requireSuccessResolver(t, resolver)
	sweeper := requireMockSweeper(t, successResolver)

	select {
	case inp := <-sweeper.sweptInputs:
		require.Equal(t, expected, inp.OutPoint())

	case <-time.After(time.Second):
		t.Fatal("expected input to be swept")
	}
}

// checkpoint holds expected data we expect the resolver to checkpoint itself
// to the DB next.
type checkpoint struct {
	// preCheckpoint is a method that will be called before we reach the
	// checkpoint, to carry out any needed operations to drive the resolver
	// in this stage.
	preCheckpoint func(*htlcResolverTestContext, bool) error

	// data we expect the resolver to be checkpointed with next.
	incubating       bool
	resolved         bool
	reports          []*channeldb.ResolverReport
	finalHtlcStored  bool
	finalHtlcSettled bool
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
		if cp.finalHtlcStored &&
			cp.finalHtlcSettled != ctx.finalHtlcSettled {

			t.Fatal("final htlc outcome expectation failed")
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
