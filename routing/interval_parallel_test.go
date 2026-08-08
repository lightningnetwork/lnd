package routing

import (
	"context"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// parallelChannel is one channel of the parallel graph below.
type parallelChannel struct {
	id       uint64
	node1    route.Vertex
	node2    route.Vertex
	capacity btcutil.Amount

	// baseFee is what the channel charges to forward, which the fee
	// budget tests need and the rest leave at zero.
	baseFee lnwire.MilliSatoshi
}

// parallelGraph is a channel graph that, unlike the mock graph the other tests
// use, can hold more than one channel between the same pair of nodes. That is
// the whole point of it: the attribution question this file tests only arises
// when a peer has a choice of channels to forward over.
type parallelGraph struct {
	channels []parallelChannel
}

// A compile time assertion that the graph satisfies both interfaces the
// interval session needs.
var _ Graph = (*parallelGraph)(nil)
var _ GraphSessionFactory = (*parallelGraph)(nil)

// ForEachNodeDirectedChannel calls the callback for every channel of the given
// node.
//
// NOTE: Part of the Graph interface.
func (g *parallelGraph) ForEachNodeDirectedChannel(_ context.Context,
	nodePub route.Vertex, cb func(*graphdb.DirectedChannel) error,
	_ func()) error {

	for _, channel := range g.channels {
		var other route.Vertex
		switch nodePub {
		case channel.node1:
			other = channel.node2

		case channel.node2:
			other = channel.node1

		default:
			continue
		}

		toNode := nodePub
		err := cb(&graphdb.DirectedChannel{
			ChannelID:    channel.id,
			OtherNode:    other,
			Capacity:     channel.capacity,
			OutPolicySet: true,
			InPolicy: &models.CachedEdgePolicy{
				ChannelID: channel.id,
				ToNodePubKey: func() route.Vertex {
					return toNode
				},
				ToNodeFeatures: lnwire.EmptyFeatureVector(),
				FeeBaseMSat:    channel.baseFee,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// FetchNodeFeatures returns the features of the given node.
//
// NOTE: Part of the Graph interface.
func (g *parallelGraph) FetchNodeFeatures(_ context.Context,
	_ route.Vertex) (*lnwire.FeatureVector, error) {

	return lnwire.EmptyFeatureVector(), nil
}

// GraphSession hands the callback access to the graph.
//
// NOTE: Part of the GraphSessionFactory interface.
func (g *parallelGraph) GraphSession(_ context.Context,
	cb func(graph graphdb.NodeTraverser) error, _ func()) error {

	return cb(g)
}

// newParallelSession builds a session over a graph in which the relay reaches
// the target over the given number of channels.
func newParallelSession(t *testing.T, siblings int, amt lnwire.MilliSatoshi) (
	*intervalPaymentSession, *IntervalStore) {

	t.Helper()

	const capacity = btcutil.Amount(1_000_000)

	var (
		source = createPubkey(sourceNodeID)
		relay  = createPubkey(firstRelayID)
		target = createPubkey(targetNodeID)
	)

	// One channel from us to the relay, then the requested number from the
	// relay onwards to the target.
	graph := &parallelGraph{
		channels: []parallelChannel{
			{id: 1, node1: source, node2: relay, capacity: capacity},
		},
	}
	for i := 0; i < siblings; i++ {
		graph.channels = append(graph.channels, parallelChannel{
			id:       uint64(100 + i),
			node1:    relay,
			node2:    target,
			capacity: capacity,
		})
	}

	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		FeeLimit:       lnwire.MaxMilliSatoshi,
		Target:         target,
		Amount:         amt,
		CltvLimit:      math.MaxUint32,
		MaxParts:       1,
		DestFeatures: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
			), lnwire.Features,
		),
	}
	require.NoError(t, payment.SetPaymentHash([32]byte{}))

	getBandwidthHints := func(_ Graph) (bandwidthHints, error) {
		return &mockBandwidthHints{
			hints: map[uint64]lnwire.MilliSatoshi{
				1: lnwire.NewMSatFromSatoshis(capacity),
			},
		}, nil
	}

	store := NewIntervalStore(0)
	session, err := newIntervalPaymentSession(
		payment, source, getBandwidthHints, graph, store,
		DefaultIntervalConfig(),
	)
	require.NoError(t, err)

	return session, store
}

// TestIntervalParallelChannelAttribution tests the invariant that guards this
// model against non-strict forwarding. A node asked to forward over one channel
// may use any channel it has to the same peer, and the onion failure that comes
// back names neither. So when a pair has several channels, a failure must never
// leave a hard bound on one of them, because the bound would be a claim the
// evidence cannot support and this model has no way to take it back.
func TestIntervalParallelChannelAttribution(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(100_000)
	session, store := newParallelSession(t, 2, amt)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	require.Len(t, rt.Hops, 2)

	// The relay refuses to forward onwards.
	failIndex := 1
	session.ReportAttemptFailure(
		0, rt, &failIndex, lnwire.NewTemporaryChannelFailure(nil),
	)

	var (
		relay    = createPubkey(firstRelayID)
		target   = createPubkey(targetNodeID)
		capacity = lnwire.NewMSatFromSatoshis(1_000_000)
	)

	// Neither channel of the pair carries a bound, because the failure
	// cannot say which of them was tried.
	for _, chanID := range []uint64{100, 101} {
		key := IntervalKey{ChanID: chanID, From: relay, To: target}

		require.Zero(t, store.Get(key, capacity).UpperFail,
			"channel %v was blamed for a failure that could not "+
				"name it", chanID)
		require.NotZero(
			t, store.Probability(key, amt, capacity),
			"channel %v was ruled out by a failure that could "+
				"not name it", chanID)
	}

	// The pair carries it instead, which is exactly what was observed:
	// this peer could not move this amount to that node.
	pair := IntervalKey{From: relay, To: target}
	require.True(t, pair.IsPairScoped())

	interval := store.Get(pair, capacity)
	require.True(t, interval.Known)
	require.NotZero(t, interval.UpperFail)
	require.LessOrEqual(t, interval.UpperFail, rt.Hops[0].AmtToForward)

	// The bound is applied on the next search, so the pair is no longer
	// offered the amount it just refused.
	require.Zero(t, store.Probability(pair, amt, capacity))

	_, err = session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.ErrorIs(t, err, errNoPathFound)
}

// TestIntervalSingleChannelKeepsChannelScope tests that the fallback above only
// fires when it has to. A pair with one channel between it is unambiguous, and
// keeping channel scope there is the whole reason this model can hold an
// interval that means something physical.
func TestIntervalSingleChannelKeepsChannelScope(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(100_000)
	session, store := newParallelSession(t, 1, amt)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)

	failIndex := 1
	session.ReportAttemptFailure(
		0, rt, &failIndex, lnwire.NewTemporaryChannelFailure(nil),
	)

	var (
		relay    = createPubkey(firstRelayID)
		target   = createPubkey(targetNodeID)
		capacity = lnwire.NewMSatFromSatoshis(1_000_000)
	)

	// The bound landed on the channel itself.
	key := IntervalKey{ChanID: 100, From: relay, To: target}
	require.NotZero(t, store.Get(key, capacity).UpperFail)

	// Nothing was written about the pair, since there was no ambiguity to
	// resolve.
	require.False(
		t, store.Get(
			IntervalKey{From: relay, To: target}, capacity,
		).Known,
	)
}

// TestIntervalScopeKey tests the rule itself.
func TestIntervalScopeKey(t *testing.T) {
	t.Parallel()

	key := IntervalKey{
		ChanID: 7,
		From:   route.Vertex{1},
		To:     route.Vertex{2},
	}

	// A pair the search never looked at, and a pair with exactly one
	// channel, are both named by their channel.
	require.Equal(t, key, intervalScopeKey(key, 0))
	require.Equal(t, key, intervalScopeKey(key, 1))
	require.False(t, key.IsPairScoped())

	// More than one channel means the observation cannot name one.
	scoped := intervalScopeKey(key, 2)
	require.True(t, scoped.IsPairScoped())
	require.Equal(t, key.From, scoped.From)
	require.Equal(t, key.To, scoped.To)

	// A pair scoped key still has two directions, since the liquidity it
	// describes still sits on one side or the other.
	require.Equal(t, key.To, scoped.Reverse().From)
	require.True(t, scoped.Reverse().IsPairScoped())
}
