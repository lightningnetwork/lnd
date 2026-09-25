package contractcourt

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// newRegistryPreimageArbitrator returns a channel arbitrator whose invoice
// registry knows the preimage for payHash while its witness beacon does not.
func newRegistryPreimageArbitrator(t *testing.T, preimage lntypes.Preimage) (
	*chanArbTestCtx, lntypes.Hash) {

	t.Helper()

	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}
	ctx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err)

	payHash := preimage.Hash()
	ctx.chanArb.cfg.PreimageDB = newMockWitnessBeacon()
	ctx.chanArb.cfg.Registry = &mockRegistry{
		invoices: map[lntypes.Hash]invoices.Invoice{
			payHash: {
				Terms: invoices.ContractTerm{
					PaymentPreimage: &preimage,
				},
			},
		},
	}

	return ctx, payHash
}

// TestIncomingRegistryPreimageGoesToChain asserts that an incoming HTLC paying
// one of our invoices takes us to chain when the invoice registry is the only
// source of its preimage, and that it counts towards the commitment deadline.
func TestIncomingRegistryPreimageGoesToChain(t *testing.T) {
	t.Parallel()

	ctx, payHash := newRegistryPreimageArbitrator(
		t, lntypes.Preimage{7, 8, 9},
	)
	chanArb := ctx.chanArb

	const refundTimeout = 100
	incomingHTLC := channeldb.HTLC{
		Incoming:      true,
		HtlcIndex:     3,
		RHash:         payHash,
		Amt:           lnwire.MilliSatoshi(50_000_000),
		RefundTimeout: refundTimeout,
		OutputIndex:   0,
	}
	htlcs := newHtlcSet([]channeldb.HTLC{incomingHTLC})

	// One block before the broadcast cutoff there is nothing to do yet.
	height := uint32(refundTimeout) - chanArb.cfg.IncomingBroadcastDelta
	actions, err := chanArb.checkCommitChainActions(
		height-1, chainTrigger, htlcs,
	)
	require.NoError(t, err)
	require.Empty(t, actions)

	// At the cutoff, the invoice preimage alone must make us go to chain,
	// handing the HTLC to a resolver that will claim it.
	actions, err = chanArb.checkCommitChainActions(
		height, chainTrigger, htlcs,
	)
	require.NoError(t, err)
	require.Equal(
		t, []channeldb.HTLC{incomingHTLC},
		actions[HtlcIncomingWatchAction],
	)

	// Without the invoice, the same HTLC gives no reason to go to chain.
	// This shows the decision above came from the registry lookup.
	registry := chanArb.cfg.Registry
	chanArb.cfg.Registry = &mockRegistry{}
	actions, err = chanArb.checkCommitChainActions(
		height, chainTrigger, htlcs,
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	chanArb.cfg.Registry = registry

	// The HTLC must also set the deadline for sweeping the commitment.
	deadline, value, err := chanArb.findCommitmentDeadlineAndValue(
		height, htlcs,
	)
	require.NoError(t, err)
	require.True(t, deadline.IsSome())
	require.Positive(t, value)

	// Without the invoice there is no time-sensitive HTLC, so no deadline.
	chanArb.cfg.Registry = &mockRegistry{}
	deadline, _, err = chanArb.findCommitmentDeadlineAndValue(
		height, htlcs,
	)
	require.NoError(t, err)
	require.True(t, deadline.IsNone())
}

// TestDanglingRegistryPreimageFails asserts that a dangling outgoing HTLC is
// classified from the witness beacon alone. A preimage known only to the
// invoice registry does not count, so the HTLC is failed back.
func TestDanglingRegistryPreimageFails(t *testing.T) {
	t.Parallel()

	ctx, payHash := newRegistryPreimageArbitrator(
		t, lntypes.Preimage{4, 5, 6},
	)
	chanArb := ctx.chanArb

	danglingHTLC := channeldb.HTLC{
		Incoming:    false,
		HtlcIndex:   77,
		RHash:       payHash,
		OutputIndex: 0,
	}
	htlcSets := map[HtlcSetKey][]channeldb.HTLC{
		LocalHtlcSet:         {},
		RemoteHtlcSet:        {danglingHTLC},
		RemotePendingHtlcSet: {},
	}

	tests := []struct {
		name      string
		trigger   transitionTrigger
		confirmed HtlcSetKey
	}{
		{
			name:      "local commitment",
			trigger:   localCloseTrigger,
			confirmed: LocalHtlcSet,
		},
		{
			name:      "remote pending commitment",
			trigger:   remoteCloseTrigger,
			confirmed: RemotePendingHtlcSet,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commitSet := CommitSet{
				ConfCommitKey: fn.Some(test.confirmed),
				HtlcSets:      htlcSets,
			}
			actions, err := chanArb.constructChainActions(
				&commitSet, 0, test.trigger,
			)
			require.NoError(t, err)
			require.Empty(t, actions[HtlcSettleDanglingAction])
			require.Equal(
				t, []channeldb.HTLC{danglingHTLC},
				actions[HtlcFailDanglingAction],
			)
		})
	}

	// settleForwards uses the same source, so it refuses the HTLC even when
	// handed it directly.
	var delivered []ResolutionMsg
	chanArb.cfg.DeliverResolutionMsg = func(msgs ...ResolutionMsg) error {
		delivered = append(delivered, msgs...)
		return nil
	}
	err := chanArb.settleForwards([]channeldb.HTLC{danglingHTLC})
	require.Error(t, err)
	require.Empty(t, delivered)
}
