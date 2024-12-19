package contractcourt

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
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
	preimage := lntypes.Preimage{}
	spendTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				Witness: [][]byte{
					{0}, {1}, {2}, preimage[:],
				},
			},
		},
	}

	spendHash := spendTx.TxHash()

	ctx.notifier.SpendChan <- &chainntnfs.SpendDetail{
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

type resolveResult struct {
	err          error
	nextResolver ContractResolver
}

type outgoingResolverTestContext struct {
	resolver           *htlcOutgoingContestResolver
	notifier           *mock.ChainNotifier
	preimageDB         *mockWitnessBeacon
	resolverResultChan chan resolveResult
	resolutionChan     chan ResolutionMsg
	t                  *testing.T
}

func newOutgoingResolverTestContext(t *testing.T) *outgoingResolverTestContext {
	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}

	checkPointChan := make(chan struct{}, 1)
	resolutionChan := make(chan ResolutionMsg, 1)

	preimageDB := newMockWitnessBeacon()

	onionProcessor := &mockOnionProcessor{}

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:   notifier,
			PreimageDB: preimageDB,
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

	return &outgoingResolverTestContext{
		resolver:       resolver,
		notifier:       notifier,
		preimageDB:     preimageDB,
		resolutionChan: resolutionChan,
		t:              t,
	}
}

func (i *outgoingResolverTestContext) resolve() {
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

	// Notify initial block height.
	i.notifyEpoch(testInitialBlockHeight)
}

func (i *outgoingResolverTestContext) notifyEpoch(height int32) {
	i.notifier.EpochChan <- &chainntnfs.BlockEpoch{
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
