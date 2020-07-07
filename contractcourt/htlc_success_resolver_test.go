package contractcourt

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var testHtlcAmt = lnwire.MilliSatoshi(200000)

type htlcSuccessResolverTestContext struct {
	resolver           *htlcSuccessResolver
	notifier           *mockNotifier
	resolverResultChan chan resolveResult
	t                  *testing.T
}

func newHtlcSuccessResolverTextContext(t *testing.T) *htlcSuccessResolverTestContext {
	notifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch),
		spendChan: make(chan *chainntnfs.SpendDetail),
		confChan:  make(chan *chainntnfs.TxConfirmation),
	}

	checkPointChan := make(chan struct{}, 1)

	testCtx := &htlcSuccessResolverTestContext{
		notifier: notifier,
		t:        t,
	}

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier: notifier,
			PublishTx: func(_ *wire.MsgTx, _ string) error {
				return nil
			},
		},
		PutResolverReport: func(_ kvdb.RwTx,
			report *channeldb.ResolverReport) error {

			return nil
		},
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint: func(_ ContractResolver,
			_ ...*channeldb.ResolverReport) error {

			checkPointChan <- struct{}{}
			return nil
		},
	}

	testCtx.resolver = &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(cfg),
		htlcResolution:      lnwallet.IncomingHtlcResolution{},
		htlc: channeldb.HTLC{
			RHash:     testResHash,
			OnionBlob: testOnionBlob,
			Amt:       testHtlcAmt,
		},
	}

	return testCtx
}

func (i *htlcSuccessResolverTestContext) resolve() {
	// Start resolver.
	i.resolverResultChan = make(chan resolveResult, 1)
	go func() {
		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

func (i *htlcSuccessResolverTestContext) waitForResult() {
	i.t.Helper()

	result := <-i.resolverResultChan
	if result.err != nil {
		i.t.Fatal(result.err)
	}

	if result.nextResolver != nil {
		i.t.Fatal("expected no next resolver")
	}
}

// TestSingleStageSuccess tests successful sweep of a single stage htlc claim.
func TestSingleStageSuccess(t *testing.T) {
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

	// We send a confirmation for our sweep tx to indicate that our sweep
	// succeeded.
	resolve := func(ctx *htlcSuccessResolverTestContext) {
		ctx.notifier.confChan <- &chainntnfs.TxConfirmation{
			Tx:          ctx.resolver.sweepTx,
			BlockHeight: testInitialBlockHeight - 1,
		}
	}

	sweepTxid := sweepTx.TxHash()
	claim := &channeldb.ResolverReport{
		OutPoint:        htlcOutpoint,
		Amount:          btcutil.Amount(testSignDesc.Output.Value),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &sweepTxid,
	}
	testHtlcSuccess(
		t, singleStageResolution, resolve, sweepTx, claim,
	)
}

// TestSecondStageResolution tests successful sweep of a second stage htlc
// claim.
func TestSecondStageResolution(t *testing.T) {
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
			TxOut: []*wire.TxOut{},
		},
		ClaimOutpoint: htlcOutpoint,
		SweepSignDesc: testSignDesc,
	}

	// We send a spend notification for our output to resolve our htlc.
	resolve := func(ctx *htlcSuccessResolverTestContext) {
		ctx.notifier.spendChan <- &chainntnfs.SpendDetail{
			SpendingTx:    sweepTx,
			SpenderTxHash: &sweepHash,
		}
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

	testHtlcSuccess(
		t, twoStageResolution, resolve, sweepTx, secondStage, firstStage,
	)
}

// testHtlcSuccess tests resolution of a success resolver. It takes a resolve
// function which triggers resolution and the sweeptxid that will resolve it.
func testHtlcSuccess(t *testing.T, resolution lnwallet.IncomingHtlcResolution,
	resolve func(*htlcSuccessResolverTestContext),
	sweepTx *wire.MsgTx, reports ...*channeldb.ResolverReport) {

	defer timeout(t)()

	ctx := newHtlcSuccessResolverTextContext(t)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolver.htlcResolution = resolution

	// We set the sweepTx to be non-nil and mark the output as already
	// incubating so that we do not need to set test values for crafting
	// our own sweep transaction.
	ctx.resolver.sweepTx = sweepTx
	ctx.resolver.outputIncubating = true

	// Start the htlc success resolver.
	ctx.resolve()

	// Trigger and event that will resolve our test context.
	resolve(ctx)

	for _, report := range reports {
		assertResolverReport(t, reportChan, report)
	}

	// Wait for the resolver to fully complete.
	ctx.waitForResult()
}
