package lnd

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
	"github.com/stretchr/testify/require"
)

// fakeCircuitSource is a test double for the switch's open circuits.
type fakeCircuitSource struct {
	circuits []*htlcswitch.PaymentCircuit
}

func (f *fakeCircuitSource) ActiveCircuits() []*htlcswitch.PaymentCircuit {
	return f.circuits
}

// fakeChannelSource is a test double for the open channel source.
type fakeChannelSource struct {
	chans []*chanstate.OpenChannel
	err   error
}

func (f *fakeChannelSource) FetchAllOpenChannels() ([]*chanstate.OpenChannel,
	error) {

	return f.chans, f.err
}

// fakeEdge is one channel's graph entry as the fake policy source serves it.
type fakeEdge struct {
	info   *models.ChannelEdgeInfo
	p1, p2 *models.ChannelEdgePolicy
}

// fakePolicySource is a test double for the channel graph.
type fakePolicySource struct {
	edges map[wire.OutPoint]fakeEdge
	err   error
}

func (f *fakePolicySource) FetchChannelEdgesByOutpoint(_ context.Context,
	op *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	if f.err != nil {
		return nil, nil, nil, f.err
	}

	edge, ok := f.edges[*op]
	if !ok {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	}

	return edge.info, edge.p1, edge.p2, nil
}

// fakeRemover records the channels it was asked to remove.
type fakeRemover struct {
	removed []lnwire.ChannelID
	err     error
}

func (f *fakeRemover) RemoveChannel(chanID lnwire.ChannelID) error {
	f.removed = append(f.removed, chanID)

	return f.err
}

// policy builds a channel policy with the given outbound base fee and rate,
// and an inbound fee if any is given.
func policy(base, rate lnwire.MilliSatoshi,
	inbound ...lnwire.Fee) *models.ChannelEdgePolicy {

	p := &models.ChannelEdgePolicy{
		FeeBaseMSat:               base,
		FeeProportionalMillionths: rate,
	}
	if len(inbound) > 0 {
		p.InboundFee = fn.Some(inbound[0])
	}

	return p
}

// makeHTLC builds a commitment HTLC with the given direction, index, expiry
// and accountable signal.
func makeHTLC(incoming bool, htlcIndex uint64, cltv uint32,
	accountable bool) chanstate.HTLC {

	records := lnwire.CustomRecords{}
	if accountable {
		records[uint64(lnwire.ExperimentalAccountableType)] =
			[]byte{lnwire.ExperimentalAccountable}
	}

	return chanstate.HTLC{
		Incoming:      incoming,
		HtlcIndex:     htlcIndex,
		RefundTimeout: cltv,
		CustomRecords: records,
	}
}

// makeChannel builds an open channel with the given scid whose HTLCs are
// locked in on both commitments, so ActiveHtlcs returns them. Its funding
// outpoint, and so its channel_id, is derived from the scid.
func makeChannel(scid uint64, htlcs ...chanstate.HTLC) *chanstate.OpenChannel {
	c := &chanstate.OpenChannel{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(scid),
		FundingOutpoint: wire.OutPoint{Index: uint32(scid)},
	}
	c.LocalCommitment.Htlcs = htlcs
	c.RemoteCommitment.Htlcs = htlcs

	return c
}

// chanIDOf returns the channel_id of the channel makeChannel builds for scid.
func chanIDOf(scid uint64) lnwire.ChannelID {
	return lnwire.NewChanIDFromOutPoint(makeChannel(scid).FundingOutpoint)
}

func inKey(scid, htlcID uint64) models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(scid),
		HtlcID: htlcID,
	}
}

func outKey(scid, htlcID uint64) *models.CircuitKey {
	k := inKey(scid, htlcID)

	return &k
}

// nextIndexes builds the channelInfo nextHtlcIndex map for makeChannel
// channels, from scid to next outgoing htlc index pairs.
func nextIndexes(pairs ...uint64) map[wire.OutPoint]uint64 {
	m := make(map[wire.OutPoint]uint64, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[makeChannel(pairs[i]).FundingOutpoint] = pairs[i+1]
	}

	return m
}

// fakeNextHtlcIndex is a nextHtlcIndexFunc serving the given map, failing for
// channels it does not know.
func fakeNextHtlcIndex(m map[wire.OutPoint]uint64) nextHtlcIndexFunc {
	return func(c *chanstate.OpenChannel) (uint64, error) {
		next, ok := m[c.FundingOutpoint]
		if !ok {
			return 0, errors.New("unknown channel")
		}

		return next, nil
	}
}

// TestAssembleInFlightHTLCs checks that open circuits are joined with the live
// incoming commitment HTLCs to recover the cltv expiry and accountable signal,
// that both ends are mapped from the circuit's short channel ids to the
// channels' channel_ids, that the fee is the offered fee, and that everything
// which is not a live forward is filtered out.
func TestAssembleInFlightHTLCs(t *testing.T) {
	t.Parallel()

	chans := []*chanstate.OpenChannel{
		// Channel 1 has two incoming HTLCs and one we offered, which
		// must not be mistaken for an incoming one with the same index.
		makeChannel(
			1,
			makeHTLC(true, 0, 500, true),
			makeHTLC(true, 1, 600, false),
			makeHTLC(false, 0, 700, true),
		),
		// Channel 2 is the outgoing channel of the forwards.
		makeChannel(2, makeHTLC(false, 7, 400, true)),
		// Channel 3 has an incoming HTLC only locked in on one
		// commitment, so it is not active.
		func() *chanstate.OpenChannel {
			c := makeChannel(3)
			c.LocalCommitment.Htlcs = []chanstate.HTLC{
				makeHTLC(true, 0, 800, true),
			}

			return c
		}(),
	}

	circuits := []*htlcswitch.PaymentCircuit{
		// Accountable forward with a 100 msat fee.
		{
			Incoming: inKey(1, 0), Outgoing: outKey(2, 7),
			IncomingAmount: 1100, OutgoingAmount: 1000,
		},
		// Unaccountable forward whose sender underpaid (impossible for
		// an accepted forward, but the fee must not go negative).
		{
			Incoming: inKey(1, 1), Outgoing: outKey(2, 8),
			IncomingAmount: 900, OutgoingAmount: 1000,
		},
		// Locally initiated payment: not a forward.
		{
			Incoming: models.CircuitKey{
				ChanID: hop.Source, HtlcID: 3,
			},
			Outgoing:       outKey(2, 9),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
		// Committed but never opened on an outgoing link.
		{
			Incoming: inKey(1, 5), IncomingAmount: 1000,
			OutgoingAmount: 1000,
		},
		// Incoming HTLC not active on the commitment any more.
		{
			Incoming: inKey(3, 0), Outgoing: outKey(2, 10),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
		// Incoming channel unknown altogether.
		{
			Incoming: inKey(9, 0), Outgoing: outKey(2, 11),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
		// Outgoing channel not open any more. Its incoming HTLC is
		// live, so only the outgoing lookup can reject it.
		{
			Incoming: inKey(1, 1), Outgoing: outKey(8, 0),
			IncomingAmount: 1100, OutgoingAmount: 1000,
		},
		// Outgoing HTLC at the channel's next index: it never made it
		// into a commitment, so the switch will forward it again.
		{
			Incoming: inKey(1, 1), Outgoing: outKey(2, 12),
			IncomingAmount: 1100, OutgoingAmount: 1000,
		},
	}

	// Without any policies the offered fee is used. Channel 2's next
	// outgoing htlc index is 12, so indexes up to 11 are committed.
	got := assembleInFlightHTLCs(circuits, chans, channelInfo{
		nextHtlcIndex: nextIndexes(2, 12),
	})
	require.Equal(t, []reputation.InFlightHTLC{
		{
			Incoming:     inKey(1, 0),
			IncomingChan: chanIDOf(1),
			OutgoingChan: chanIDOf(2),
			Fee:          100,
			IncomingCltv: 500,
			Accountable:  true,
		},
		{
			Incoming:     inKey(1, 1),
			IncomingChan: chanIDOf(1),
			OutgoingChan: chanIDOf(2),
			Fee:          0,
			IncomingCltv: 600,
			Accountable:  false,
		},
	}, got)

	// The channel_ids come from the funding outpoints, not the scids: two
	// channels must not collide just because makeChannel derives one from
	// the other.
	require.NotEqual(t, chanIDOf(1), chanIDOf(2))
}

// TestAssembleInFlightHTLCsRequiredFee checks that a replayed HTLC is scored
// on the fee the node's own policies require, computed like the live forward
// path (outbound fee of the outgoing channel plus inbound fee of the incoming
// one), capped at the fee the sender offered and clamped at zero.
func TestAssembleInFlightHTLCsRequiredFee(t *testing.T) {
	t.Parallel()

	// Both channels of a 1 -> 2 forward of 100_000 msat, with the sender
	// offering a 1000 msat fee.
	chans := []*chanstate.OpenChannel{
		makeChannel(1, makeHTLC(true, 0, 500, false)),
		makeChannel(2),
	}
	circuit := func() *htlcswitch.PaymentCircuit {
		return &htlcswitch.PaymentCircuit{
			Incoming: inKey(1, 0), Outgoing: outKey(2, 0),
			IncomingAmount: 101_000, OutgoingAmount: 100_000,
		}
	}

	tests := []struct {
		name     string
		policies map[wire.OutPoint]*models.ChannelEdgePolicy
		wantFee  lnwire.MilliSatoshi
	}{{
		// 100 base + 100_000 * 1000 / 1e6 = 200 outbound, no inbound.
		name: "required below offered",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[0].FundingOutpoint: policy(0, 0),
			chans[1].FundingOutpoint: policy(100, 1000),
		},
		wantFee: 200,
	}, {
		// Inbound fee of 50 base + 1000 ppm on (100_000 + 200) is
		// charged on top: 200 + 50 + 100 = 350.
		name: "inbound fee included",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[0].FundingOutpoint: policy(
				0, 0, lnwire.Fee{BaseFee: 50, FeeRate: 1000},
			),
			chans[1].FundingOutpoint: policy(100, 1000),
		},
		wantFee: 350,
	}, {
		// An inbound discount larger than the outbound fee clamps at
		// zero rather than going negative.
		name: "inbound discount clamps at zero",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[0].FundingOutpoint: policy(
				0, 0, lnwire.Fee{BaseFee: -1000},
			),
			chans[1].FundingOutpoint: policy(100, 1000),
		},
		wantFee: 0,
	}, {
		// The policy was raised since the forward was accepted: the
		// node still cannot earn more than the sender offered.
		name: "required capped at offered",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[0].FundingOutpoint: policy(0, 0),
			chans[1].FundingOutpoint: policy(5000, 0),
		},
		wantFee: 1000,
	}, {
		// Only the outgoing policy is known: fall back to offered.
		name: "incoming policy missing",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[1].FundingOutpoint: policy(100, 1000),
		},
		wantFee: 1000,
	}, {
		// Only the incoming policy is known: fall back to offered.
		name: "outgoing policy missing",
		policies: map[wire.OutPoint]*models.ChannelEdgePolicy{
			chans[0].FundingOutpoint: policy(0, 0),
		},
		wantFee: 1000,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := assembleInFlightHTLCs(
				[]*htlcswitch.PaymentCircuit{circuit()}, chans,
				channelInfo{
					policies:      test.policies,
					nextHtlcIndex: nextIndexes(2, 1),
				},
			)
			require.Len(t, got, 1)
			require.Equal(t, test.wantFee, got[0].Fee)
		})
	}
}

// TestFetchSelfPolicies checks that the node's own policy is picked from the
// two directional policies of each channel, that channels without a graph
// entry or without our policy are left out, and that other graph errors are
// surfaced.
func TestFetchSelfPolicies(t *testing.T) {
	t.Parallel()

	var self, other [33]byte
	self[0], other[0] = 2, 3

	chans := []*chanstate.OpenChannel{
		makeChannel(1), makeChannel(2), makeChannel(3), makeChannel(4),
	}
	ours1, ours2 := policy(1, 0), policy(2, 0)
	theirs := policy(99, 0)

	graph := &fakePolicySource{
		edges: map[wire.OutPoint]fakeEdge{
			// We are node 1, so ours is the first policy.
			chans[0].FundingOutpoint: {
				info: &models.ChannelEdgeInfo{
					NodeKey1Bytes: self,
					NodeKey2Bytes: other,
				},
				p1: ours1, p2: theirs,
			},
			// We are node 2, so ours is the second policy.
			chans[1].FundingOutpoint: {
				info: &models.ChannelEdgeInfo{
					NodeKey1Bytes: other,
					NodeKey2Bytes: self,
				},
				p1: theirs, p2: ours2,
			},
			// Our policy was never advertised.
			chans[2].FundingOutpoint: {
				info: &models.ChannelEdgeInfo{
					NodeKey1Bytes: self,
					NodeKey2Bytes: other,
				},
				p2: theirs,
			},
			// Channel 4 is not in the graph at all.
		},
	}

	got, err := fetchSelfPolicies(t.Context(), graph, self, chans)
	require.NoError(t, err)
	require.Equal(t, map[wire.OutPoint]*models.ChannelEdgePolicy{
		chans[0].FundingOutpoint: ours1,
		chans[1].FundingOutpoint: ours2,
	}, got)

	// Any error other than a missing edge must not be mistaken for "no
	// policies".
	graph.err = errors.New("db closed")
	_, err = fetchSelfPolicies(t.Context(), graph, self, chans)
	require.ErrorIs(t, err, graph.err)
}

// TestRemoveClosedChannel checks that a closed channel event removes the
// channel by the channel_id of its channel point, that other channel events
// and a closed channel event without a summary are ignored, and that a
// failure to remove is not propagated.
func TestRemoveClosedChannel(t *testing.T) {
	t.Parallel()

	mgr := &fakeRemover{}
	chanPoint := wire.OutPoint{Index: 7}

	removeClosedChannel(mgr, channelnotifier.OpenChannelEvent{})
	removeClosedChannel(mgr, channelnotifier.ClosedChannelEvent{})
	require.Empty(t, mgr.removed)

	removeClosedChannel(mgr, channelnotifier.ClosedChannelEvent{
		CloseSummary: &chanstate.ChannelCloseSummary{
			ChanPoint: chanPoint,
		},
	})
	require.Equal(t, []lnwire.ChannelID{
		lnwire.NewChanIDFromOutPoint(chanPoint),
	}, mgr.removed)

	mgr.err = errors.New("store closed")
	removeClosedChannel(mgr, channelnotifier.ClosedChannelEvent{
		CloseSummary: &chanstate.ChannelCloseSummary{
			ChanPoint: chanPoint,
		},
	})
	require.Len(t, mgr.removed, 2)
}

// TestReconstructInFlightHTLCs checks the sources are read through, and that a
// channel source error is surfaced rather than treated as no channels.
func TestReconstructInFlightHTLCs(t *testing.T) {
	t.Parallel()

	circuits := &fakeCircuitSource{
		circuits: []*htlcswitch.PaymentCircuit{{
			Incoming: inKey(1, 0), Outgoing: outKey(2, 0),
			IncomingAmount: 1010, OutgoingAmount: 1000,
		}},
	}
	chans := &fakeChannelSource{
		chans: []*chanstate.OpenChannel{
			makeChannel(1, makeHTLC(true, 0, 500, true)),
			makeChannel(2),
		},
	}

	var self [33]byte
	self[0] = 2
	graph := &fakePolicySource{
		edges: map[wire.OutPoint]fakeEdge{
			// Our policy on the outgoing channel requires 5 msat,
			// half of the 10 offered.
			chans.chans[1].FundingOutpoint: {
				info: &models.ChannelEdgeInfo{
					NodeKey1Bytes: self,
				},
				p1: policy(5, 0),
			},
			chans.chans[0].FundingOutpoint: {
				info: &models.ChannelEdgeInfo{
					NodeKey1Bytes: self,
				},
				p1: policy(0, 0),
			},
		},
	}

	nextIndex := fakeNextHtlcIndex(nextIndexes(1, 0, 2, 1))

	got, err := reconstructInFlightHTLCs(
		t.Context(), circuits, chans, graph, self, nextIndex,
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.EqualValues(t, 5, got[0].Fee)
	require.True(t, got[0].Accountable)
	require.Equal(t, chanIDOf(1), got[0].IncomingChan)
	require.Equal(t, chanIDOf(2), got[0].OutgoingChan)

	// An error reading the channels or the policies must not be mistaken
	// for "no HTLCs in flight".
	chans.err = errors.New("db closed")
	_, err = reconstructInFlightHTLCs(
		t.Context(), circuits, chans, graph, self, nextIndex,
	)
	require.ErrorIs(t, err, chans.err)

	chans.err = nil
	graph.err = errors.New("graph closed")
	_, err = reconstructInFlightHTLCs(
		t.Context(), circuits, chans, graph, self, nextIndex,
	)
	require.ErrorIs(t, err, graph.err)

	// A channel whose next htlc index cannot be read is an error too.
	graph.err = nil
	_, err = reconstructInFlightHTLCs(
		t.Context(), circuits, chans, graph, self,
		fakeNextHtlcIndex(nil),
	)
	require.ErrorContains(t, err, "next htlc index")

	// No open circuits means nothing to replay.
	circuits.circuits = nil
	got, err = reconstructInFlightHTLCs(
		t.Context(), circuits, chans, graph, self, nextIndex,
	)
	require.NoError(t, err)
	require.Empty(t, got)
}
