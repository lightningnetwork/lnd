package contractcourt

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var testHtlcAmt = lnwire.MilliSatoshi(200000)

type htlcSuccessResolverTestContext struct {
	resolver           *htlcSuccessResolver
	notifier           *mock.ChainNotifier
	resolverResultChan chan resolveResult
	t                  *testing.T
}

func newHtlcSuccessResolverTextContext(t *testing.T, checkpoint io.Reader) *htlcSuccessResolverTestContext {
	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		ConfChan:  make(chan *chainntnfs.TxConfirmation, 1),
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
			Sweeper: newMockSweeper(),
			IncubateOutputs: func(wire.OutPoint, *lnwallet.OutgoingHtlcResolution,
				*lnwallet.IncomingHtlcResolution, uint32) error {
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
	htlc := channeldb.HTLC{
		RHash:     testResHash,
		OnionBlob: testOnionBlob,
		Amt:       testHtlcAmt,
	}
	if checkpoint != nil {
		var err error
		testCtx.resolver, err = newSuccessResolverFromReader(checkpoint, cfg)
		if err != nil {
			t.Fatal(err)
		}

		testCtx.resolver.Supplement(htlc)

	} else {

		testCtx.resolver = &htlcSuccessResolver{
			contractResolverKit: *newContractResolverKit(cfg),
			htlcResolution:      lnwallet.IncomingHtlcResolution{},
			htlc:                htlc,
		}
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
			preCheckpoint: func(ctx *htlcSuccessResolverTestContext,
				_ bool) error {
				// The resolver will create and publish a sweep
				// tx.
				ctx.resolver.Sweeper.(*mockSweeper).
					createSweepTxChan <- sweepTx

				// Confirm the sweep, which should resolve it.
				ctx.notifier.ConfChan <- &chainntnfs.TxConfirmation{
					Tx:          sweepTx,
					BlockHeight: testInitialBlockHeight - 1,
				}

				return nil
			},

			// After the sweep has confirmed, we expect the
			// checkpoint to be resolved, and with the above
			// report.
			resolved: true,
			reports: []*channeldb.ResolverReport{
				claim,
			},
		},
	}

	testHtlcSuccess(
		t, singleStageResolution, checkpoints,
	)
}

// TestSecondStageResolution tests successful sweep of a second stage htlc
// claim, going through the Nursery.
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
			preCheckpoint: func(ctx *htlcSuccessResolverTestContext,
				_ bool) error {
				ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
					SpendingTx:    sweepTx,
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
		},
	}

	testHtlcSuccess(
		t, twoStageResolution, checkpoints,
	)
}

// checkpoint holds expected data we expect the resolver to checkpoint itself
// to the DB next.
type checkpoint struct {
	// preCheckpoint is a method that will be called before we reach the
	// checkpoint, to carry out any needed operations to drive the resolver
	// in this stage.
	preCheckpoint func(*htlcSuccessResolverTestContext, bool) error

	// data we expect the resolver to be checkpointed with next.
	incubating bool
	resolved   bool
	reports    []*channeldb.ResolverReport
}

// testHtlcSuccess tests resolution of a success resolver. It takes a a list of
// checkpoints that it expects the resolver to go through. And will run the
// resolver all the way through these checkpoints, and also attempt to resume
// the resolver from every checkpoint.
func testHtlcSuccess(t *testing.T, resolution lnwallet.IncomingHtlcResolution,
	checkpoints []checkpoint) {

	defer timeout(t)()

	// We first run the resolver from start to finish, ensuring it gets
	// checkpointed at every expected stage. We store the checkpointed data
	// for the next portion of the test.
	ctx := newHtlcSuccessResolverTextContext(t, nil)
	ctx.resolver.htlcResolution = resolution

	checkpointedState := runFromCheckpoint(t, ctx, checkpoints)

	// Now, from every checkpoint created, we re-create the resolver, and
	// run the test from that checkpoint.
	for i := range checkpointedState {
		cp := bytes.NewReader(checkpointedState[i])
		ctx := newHtlcSuccessResolverTextContext(t, cp)
		ctx.resolver.htlcResolution = resolution

		// Run from the given checkpoint, ensuring we'll hit the rest.
		_ = runFromCheckpoint(t, ctx, checkpoints[i+1:])
	}
}

// runFromCheckpoint executes the Resolve method on the success resolver, and
// asserts that it checkpoints itself according to the expected checkpoints.
func runFromCheckpoint(t *testing.T, ctx *htlcSuccessResolverTestContext,
	expectedCheckpoints []checkpoint) [][]byte {

	defer timeout(t)()

	var checkpointedState [][]byte

	// Replace our checkpoint method with one which we'll use to assert the
	// checkpointed state and reports are equal to what we expect.
	nextCheckpoint := 0
	checkpointChan := make(chan struct{})
	ctx.resolver.Checkpoint = func(resolver ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		if nextCheckpoint >= len(expectedCheckpoints) {
			t.Fatal("did not expect more checkpoints")
		}

		h := resolver.(*htlcSuccessResolver)
		cp := expectedCheckpoints[nextCheckpoint]

		if h.resolved != cp.resolved {
			t.Fatalf("expected checkpoint to be resolve=%v, had %v",
				cp.resolved, h.resolved)
		}

		if !reflect.DeepEqual(h.outputIncubating, cp.incubating) {
			t.Fatalf("expected checkpoint to be have "+
				"incubating=%v, had %v", cp.incubating,
				h.outputIncubating)

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

		// Finally encode the resolver, and store it for later use.
		b := bytes.Buffer{}
		if err := resolver.Encode(&b); err != nil {
			t.Fatal(err)
		}

		checkpointedState = append(checkpointedState, b.Bytes())
		nextCheckpoint++
		checkpointChan <- struct{}{}
		return nil
	}

	// Start the htlc success resolver.
	ctx.resolve()

	// Go through our list of expected checkpoints, so we can run the
	// preCheckpoint logic if needed.
	resumed := true
	for i, cp := range expectedCheckpoints {
		if cp.preCheckpoint != nil {
			if err := cp.preCheckpoint(ctx, resumed); err != nil {
				t.Fatalf("failure at stage %d: %v", i, err)
			}

		}
		resumed = false

		// Wait for the resolver to have checkpointed its state.
		<-checkpointChan
	}

	// Wait for the resolver to fully complete.
	ctx.waitForResult()

	if nextCheckpoint < len(expectedCheckpoints) {
		t.Fatalf("not all checkpoints hit")
	}

	return checkpointedState
}
