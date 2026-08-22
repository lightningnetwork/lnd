package contractcourt

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	outgoingContestHtlcExpiry = 110

	// outgoingContestIncomingHtlcExpiry is kept distinct from the outgoing
	// HTLC expiry to verify that the supplied value is retained.
	outgoingContestIncomingHtlcExpiry = 144
)

// TestHtlcOutgoingResolverTimeout tests resolution of an offered htlc that
// timed out.
func TestHtlcOutgoingResolverTimeout(t *testing.T) {
	t.Parallel()
	defer timeout()()

	// Setup the resolver with our test resolution.
	ctx := newOutgoingResolverTestContext(t)

	// Start the resolution process in a goroutine.
	ctx.resolve()

	// Notify arrival of the block after which the timeout path of the htlc
	// unlocks.
	ctx.notifyEpoch(outgoingContestHtlcExpiry)

	// Assert that the resolver finishes without error and transforms in a
	// timeout resolver.
	ctx.waitForResult(true)
}

// TestHtlcOutgoingResolverRemoteClaim tests resolution of an offered htlc that
// is claimed by the remote party.
func TestHtlcOutgoingResolverRemoteClaim(t *testing.T) {
	t.Parallel()
	defer timeout()()

	// Setup the resolver with our test resolution and start the resolution
	// process.
	ctx := newOutgoingResolverTestContext(t)

	// Replace our mocked checkpoint function with one which will push
	// reports into a channel for us to consume. We do so on the resolver
	// level because our test context has already created the resolver.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()

	// The remote party sweeps the htlc. Notify our resolver of this event.
	// The revealed preimage has to open this HTLC's payment hash, as that's
	// the only thing that makes the spend a claim rather than some other
	// spend of the output.
	preimage := testResPreimage
	spendTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: ctx.resolver.htlcResolution.
					ClaimOutpoint,
				Witness: [][]byte{
					{0}, {1}, {2}, preimage[:],
					{txscript.OP_TRUE},
				},
			},
		},
	}

	spendHash := spendTx.TxHash()

	ctx.spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:    spendTx,
		SpenderTxHash: &spendHash,
	}

	// We expect the extracted preimage to be added to the witness beacon.
	<-ctx.preimageDB.newPreimages

	// We also expect a resolution message to the incoming side of the
	// circuit.
	<-ctx.resolutionChan

	// Finally, check that we have a report as expected.
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        wire.OutPoint{},
		Amount:          0,
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendHash,
	}

	assertResolverReport(t, reportChan, expectedReport)

	// Assert that the resolver finishes without error.
	ctx.waitForResult(false)
}

// TestHtlcOutgoingResolverTaprootSpend tests both contest spend notification
// sites with authenticated and non-preimage Taproot paths.
func TestHtlcOutgoingResolverTaprootSpend(t *testing.T) {
	// Arrange: Build cached and live success, alternate, and malformed
	// spends of the authoritative Taproot HTLC output.
	// Act: Launch resolution and deliver each spend through the buffered
	// mock event while recording the output that the resolver watches.
	// Assert: Only authenticated success resolves and reveals a preimage;
	// other paths transition safely and malformed proofs return an error.
	tests := []struct {
		name      string
		cached    bool
		spendPath string
	}{
		{"cached success", true, "success"},
		{"cached auxiliary", true, "auxiliary"},
		{"cached key", true, "key"},
		{"cached malformed", true, "malformed"},
		{"loop success", false, "success"},
		{"loop auxiliary", false, "auxiliary"},
		{"loop key", false, "key"},
		{"loop malformed", false, "malformed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := newOutgoingResolverTestContext(t)
			fixture := newOutgoingTaprootFixture(t, test.spendPath)
			ctx.resolver.htlcResolution = fixture.resolution
			ctx.resolver.initReport()
			initialReport := ctx.resolver.currentReport

			if test.cached {
				ctx.spendChan <- fixture.spend
				ctx.startResolve()
			} else {
				ctx.resolve()
				ctx.spendChan <- fixture.spend
			}

			registration := <-ctx.registered
			require.Equal(
				t, fixture.outpoint, registration.outpoint,
			)
			require.Equal(
				t, fixture.pkScript, registration.pkScript,
			)

			result := <-ctx.resolverResultChan
			switch test.spendPath {
			case "success":
				require.NoError(t, result.err)
				require.Nil(t, result.nextResolver)
				require.True(t, ctx.resolver.IsResolved())
				require.Len(t, ctx.preimageDB.newPreimages, 1)
				require.Len(t, ctx.resolutionChan, 1)
				require.Len(t, ctx.checkpointChan, 1)

			case "malformed":
				require.Error(t, result.err)
				require.NotErrorIs(
					t, result.err, errPreimageMismatch,
				)
				require.Nil(t, result.nextResolver)

			default:
				require.NoError(t, result.err)
				require.Same(
					t, ctx.resolver.htlcTimeoutResolver,
					result.nextResolver,
				)
			}

			if test.spendPath != "success" {
				require.False(t, ctx.resolver.IsResolved())
				require.Empty(t, ctx.preimageDB.newPreimages)
				require.Empty(t, ctx.resolutionChan)
				require.Empty(t, ctx.checkpointChan)
				require.Empty(
					t, ctx.htlcNotifier.finalHtlcEvents,
				)
				require.Equal(
					t, initialReport,
					ctx.resolver.currentReport,
				)
			}
		})
	}
}

// outgoingTaprootFixture pairs a commitment resolution with its candidate
// spend so contest tests exercise the same authenticated tree and outpoint.
type outgoingTaprootFixture struct {
	resolution lnwallet.OutgoingHtlcResolution
	spend      *chainntnfs.SpendDetail
	outpoint   wire.OutPoint
	pkScript   []byte
}

// newOutgoingTaprootFixture creates a realistic remote-commitment Taproot
// spend and the resolution data needed to authenticate it.
func newOutgoingTaprootFixture(t *testing.T,
	spendPath string) *outgoingTaprootFixture {

	t.Helper()
	_, senderKey := btcec.PrivKeyFromBytes([]byte{1})
	_, receiverKey := btcec.PrivKeyFromBytes([]byte{2})
	_, revokeKey := btcec.PrivKeyFromBytes([]byte{3})
	successLeaf, err := input.ReceiverHtlcTapLeafSuccess(
		receiverKey, senderKey, testResHash[:],
	)
	require.NoError(t, err)
	auxLeaf := txscript.NewTapLeaf(0xc2, successLeaf.Script)
	tree, err := input.ReceiverHTLCScriptTaproot(
		outgoingContestHtlcExpiry, senderKey, receiverKey, revokeKey,
		testResHash[:], lntypes.Remote, fn.Some(auxLeaf),
	)
	require.NoError(t, err)

	controlBytes := func(path input.ScriptPath) []byte {
		control, err := tree.CtrlBlockForPath(path)
		require.NoError(t, err)
		serialized, err := control.ToBytes()
		require.NoError(t, err)

		return serialized
	}
	timeoutControl := controlBytes(input.ScriptPathTimeout)
	script, control := tree.SuccessTapLeaf.Script,
		controlBytes(input.ScriptPathSuccess)
	preimage := testResPreimage[:]
	switch spendPath {
	case "auxiliary":
		auxIndex := tree.TapScriptTree().
			LeafProofIndex[auxLeaf.TapHash()]
		auxProof := tree.TapScriptTree().LeafMerkleProofs[auxIndex]
		auxControl := auxProof.ToControlBlock(revokeKey)
		control, err = auxControl.ToBytes()
		require.NoError(t, err)
		script = auxLeaf.Script
		preimage = make([]byte, lntypes.HashSize)

	case "key":
		script, control = nil, nil

	case "malformed":
		control = []byte{1}
	}

	outpoint := wire.OutPoint{Index: 9}
	witness := wire.TxWitness{
		dummyBytes, dummyBytes, preimage, script, control,
	}
	if spendPath == "key" {
		witness = wire.TxWitness{dummyBytes}
	}
	spendingTx := &wire.MsgTx{TxIn: []*wire.TxIn{{
		PreviousOutPoint: outpoint,
		Witness:          witness,
	}}}
	spendingHash := spendingTx.TxHash()
	pkScript := tree.PkScript()

	return &outgoingTaprootFixture{
		resolution: lnwallet.OutgoingHtlcResolution{
			ClaimOutpoint: outpoint,
			Expiry:        outgoingContestHtlcExpiry,
			SweepSignDesc: input.SignDescriptor{
				Output: &wire.TxOut{
					Value:    int64(testHtlcAmount),
					PkScript: pkScript,
				},
				WitnessScript: tree.TimeoutTapLeaf.Script,
				ControlBlock:  timeoutControl,
			},
		},
		spend: &chainntnfs.SpendDetail{
			SpentOutPoint:     &outpoint,
			SpendingTx:        spendingTx,
			SpenderTxHash:     &spendingHash,
			SpenderInputIndex: 0,
		},
		outpoint: outpoint,
		pkScript: pkScript,
	}
}

type resolveResult struct {
	err          error
	nextResolver ContractResolver
}

// TestHtlcOutgoingResolverSupplementDeadline checks that the outgoing contest
// resolver forwards the incoming HTLC deadline to the timeout resolver it
// transitions into once the outgoing HTLC expires on-chain.
func TestHtlcOutgoingResolverSupplementDeadline(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newOutgoingResolverTestContext(t)

	// Initially the embedded timeout resolver carries no deadline.
	require.True(t, ctx.resolver.incomingHTLCExpiryHeight.IsNone())

	// Supply the deadline through the contest resolver, as the channel
	// arbitrator does when constructing the resolver.
	deadline := fn.Some(int32(outgoingContestIncomingHtlcExpiry))
	ctx.resolver.SupplementDeadline(deadline)

	// Drive the contest resolver to the point where it returns the embedded
	// timeout resolver.
	ctx.resolve()
	ctx.notifyEpoch(outgoingContestHtlcExpiry)

	result := <-ctx.resolverResultChan
	require.NoError(t, result.err)

	timeoutRes, ok := result.nextResolver.(*htlcTimeoutResolver)
	require.True(t, ok, "expected htlcTimeoutResolver")
	require.Equal(t, deadline, timeoutRes.incomingHTLCExpiryHeight)
}

type outgoingResolverTestContext struct {
	resolver           *htlcOutgoingContestResolver
	notifier           *chainntnfs.MockChainNotifier
	spendChan          chan *chainntnfs.SpendDetail
	epochChan          chan *chainntnfs.BlockEpoch
	registered         chan spendRegistration
	preimageDB         *mockWitnessBeacon
	htlcNotifier       *mockHTLCNotifier
	resolverResultChan chan resolveResult
	resolutionChan     chan ResolutionMsg
	checkpointChan     chan struct{}
	t                  *testing.T
}

func newOutgoingResolverTestContext(t *testing.T) *outgoingResolverTestContext {
	notifier, spendChan, epochChan, registered := newSpendMockNotifier()

	checkPointChan := make(chan struct{}, 1)
	resolutionChan := make(chan ResolutionMsg, 1)

	preimageDB := newMockWitnessBeacon()
	htlcNotifier := &mockHTLCNotifier{}

	onionProcessor := &mockOnionProcessor{}

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:     notifier,
			PreimageDB:   preimageDB,
			HtlcNotifier: htlcNotifier,
			DeliverResolutionMsg: func(msgs ...ResolutionMsg) error {
				if len(msgs) != 1 {
					return fmt.Errorf("expected 1 "+
						"resolution msg, instead got %v",
						len(msgs))
				}

				resolutionChan <- msgs[0]
				return nil
			},
			OnionProcessor: onionProcessor,
			Budget:         *DefaultBudgetConfig(),
			QueryIncomingCircuit: func(
				circuit models.CircuitKey) *models.CircuitKey {

				return nil
			},
			ChainIO: &mock.ChainIO{},
		},
		PutResolverReport: func(_ kvdb.RwTx,
			_ *channeldb.ResolverReport) error {

			return nil
		},
	}

	outgoingRes := lnwallet.OutgoingHtlcResolution{
		Expiry: outgoingContestHtlcExpiry,
		SweepSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{},
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

	resolver := &htlcOutgoingContestResolver{
		htlcTimeoutResolver: &htlcTimeoutResolver{
			contractResolverKit: *newContractResolverKit(cfg),
			htlcResolution:      outgoingRes,
			htlc: channeldb.HTLC{
				Amt:       lnwire.MilliSatoshi(testHtlcAmount),
				RHash:     testResHash,
				OnionBlob: lnmock.MockOnion(),
			},
		},
	}
	resolver.initLogger("htlcOutgoingContestResolver")
	resolver.initReport()

	return &outgoingResolverTestContext{
		resolver:       resolver,
		notifier:       notifier,
		spendChan:      spendChan,
		epochChan:      epochChan,
		registered:     registered,
		preimageDB:     preimageDB,
		htlcNotifier:   htlcNotifier,
		resolutionChan: resolutionChan,
		checkpointChan: checkPointChan,
		t:              t,
	}
}

// startResolve starts the contest resolver without sending an initial epoch
// and returns every launch or resolution error to the owning test goroutine.
func (i *outgoingResolverTestContext) startResolve() {
	i.resolverResultChan = make(chan resolveResult, 1)
	go func() {
		err := i.resolver.Launch()
		if err != nil {
			i.resolverResultChan <- resolveResult{err: err}
			return
		}

		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

// resolve starts the contest resolver and sends its initial block epoch.
func (i *outgoingResolverTestContext) resolve() {
	i.startResolve()

	// Notify initial block height.
	i.notifyEpoch(testInitialBlockHeight)
}

// notifyEpoch delivers a block height through the explicit mock event so the
// resolver observes it through the production notifier subscription.
func (i *outgoingResolverTestContext) notifyEpoch(height int32) {
	i.epochChan <- &chainntnfs.BlockEpoch{
		Height: height,
	}
}

func (i *outgoingResolverTestContext) waitForResult(expectTimeoutRes bool) {
	i.t.Helper()

	result := <-i.resolverResultChan
	if result.err != nil {
		i.t.Fatal(result.err)
	}

	if !expectTimeoutRes {
		if result.nextResolver != nil {
			i.t.Fatal("expected no next resolver")
		}
		return
	}

	_, ok := result.nextResolver.(*htlcTimeoutResolver)
	if !ok {
		i.t.Fatal("expected htlcTimeoutResolver")
	}
}
