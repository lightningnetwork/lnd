package routing

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// Node ids for the in-flight tests, on top of the ones the other interval
// tests already define.
const (
	thirdRelayID  = 5
	fourthRelayID = 6
)

// newCorridorSession builds a session over a graph offering two disjoint two
// hop corridors from us to the target, and returns the session and the node
// wide store behind it.
//
// The corridors are deliberately identical in capacity and policy, so that
// nothing but liquidity beliefs can separate them.
func newCorridorSession(t *testing.T, amt lnwire.MilliSatoshi,
	maxParts uint32) (*intervalPaymentSession, *IntervalStore) {

	t.Helper()

	const capacity = btcutil.Amount(1_000_000)

	var (
		source = createPubkey(sourceNodeID)
		first  = createPubkey(firstRelayID)
		second = createPubkey(secondRelayID)
		target = createPubkey(targetNodeID)
	)

	graph := &parallelGraph{
		channels: []parallelChannel{
			{id: 1, node1: source, node2: first, capacity: capacity},
			{id: 2, node1: first, node2: target, capacity: capacity},
			{id: 3, node1: source, node2: second, capacity: capacity},
			{id: 4, node1: second, node2: target, capacity: capacity},
		},
	}

	var paymentAddr [32]byte
	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		FeeLimit:       lnwire.MaxMilliSatoshi,
		Target:         target,
		PaymentAddr:    fn.Some(paymentAddr),
		Amount:         amt,
		CltvLimit:      math.MaxUint32,
		MaxParts:       maxParts,
		DestFeatures: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
				lnwire.PaymentAddrOptional,
				lnwire.MPPOptional,
			), lnwire.Features,
		),
	}
	require.NoError(t, payment.SetPaymentHash([32]byte{}))

	getBandwidthHints := func(_ Graph) (bandwidthHints, error) {
		return &mockBandwidthHints{
			hints: map[uint64]lnwire.MilliSatoshi{
				1: lnwire.NewMSatFromSatoshis(capacity),
				3: lnwire.NewMSatFromSatoshis(capacity),
			},
		}, nil
	}

	store := NewIntervalStore(0)

	cfg := DefaultIntervalConfig()
	cfg.MinShardAmt = lnwire.NewMSatFromSatoshis(1_000)

	session, err := newIntervalPaymentSession(
		payment, source, getBandwidthHints, graph, store, cfg,
	)
	require.NoError(t, err)

	return session, store
}

// relayOf returns the relay a two hop route went through.
func relayOf(t *testing.T, rt *route.Route) route.Vertex {
	t.Helper()

	require.Len(t, rt.Hops, 2)

	return rt.Hops[0].PubKeyBytes
}

// TestIntervalInFlightPricesOwnHolds tests that a second shard prices an
// interior corridor knowing what the first shard is already holding on it.
//
// Nothing in the graph distinguishes the two corridors, and the sender's own
// channels are covered by the bandwidth hints, so the only way the second shard
// can prefer the untouched corridor is by counting the HTLC it already has in
// flight on the other one.
func TestIntervalInFlightPricesOwnHolds(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	session, store := newCorridorSession(t, amt, 4)

	// The first shard takes one of the two corridors.
	first, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	firstRelay := relayOf(t, first)

	// It is now holding the interior hop of that corridor, and the store
	// says so to anyone who asks.
	interior := IntervalKey{
		ChanID: intervalChanIDOf(first, 1),
		From:   firstRelay,
		To:     createPubkey(targetNodeID),
	}
	require.Equal(t, first.Hops[0].AmtToForward, store.Held(interior))

	// The second shard, asked for while the first is still in flight, takes
	// the other corridor.
	second, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 1, 0, nil)
	require.NoError(t, err)
	require.NotEqual(t, firstRelay, relayOf(t, second))

	// The preference is the hold and nothing else: with the hold released,
	// the search is free to return the first corridor again.
	session.ReportAttemptFailure(
		0, first, nil, lnwire.NewTemporaryChannelFailure(nil),
	)
	require.Zero(t, store.Held(interior))
}

// TestIntervalInFlightRaisesEffectiveAmount tests the arithmetic underneath the
// preference above. A hop holding H of ours must have had A plus H available
// when we last looked at it, so a hold can rule a shard out on its own.
func TestIntervalInFlightRaisesEffectiveAmount(t *testing.T) {
	t.Parallel()

	capacity := lnwire.NewMSatFromSatoshis(1_000_000)
	amt := capacity / 2

	session, store := newCorridorSession(t, amt, 4)

	key := IntervalKey{
		ChanID: 2,
		From:   createPubkey(firstRelayID),
		To:     createPubkey(targetNodeID),
	}

	// With nothing held, the hop is priced on the prior alone.
	cold := session.edgeProbability(key, amt, capacity)
	require.Greater(t, cold, 0.0)

	// We have watched the hop carry the whole amount, so it is near
	// certain.
	store.RecordProbe(key, amt, capacity)
	require.EqualValues(
		t, intervalProvenProbability,
		session.edgeProbability(key, amt, capacity),
	)

	// Now hold half the amount on it. The hop is no longer being asked for
	// something we have proven, it is being asked for half again as much,
	// and the price drops accordingly.
	store.Hold(map[IntervalKey]lnwire.MilliSatoshi{key: amt / 2})

	withHold := session.edgeProbability(key, amt, capacity)
	require.Less(t, withHold, intervalProvenProbability)

	// A hold that takes the pair past what we have watched fail rules the
	// shard out entirely, without the router having to spend an attempt to
	// find that out.
	store.RecordFailure(key, amt+amt/2, capacity)
	require.Zero(t, session.edgeProbability(key, amt, capacity))
}

// TestIntervalInFlightIgnoresFirstHop tests that we do not charge our own
// channels twice. The switch already nets in-flight HTLCs out of the bandwidth
// it reports for our links, so the pathfinder has heard about them once
// already.
func TestIntervalInFlightIgnoresFirstHop(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	session, store := newCorridorSession(t, amt, 4)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)

	// The interior hop is held, the hop out of our own node is not.
	own := IntervalKey{
		ChanID: rt.Hops[0].ChannelID,
		From:   createPubkey(sourceNodeID),
		To:     relayOf(t, rt),
	}
	require.Zero(t, store.Held(own))
	require.NotZero(t, store.Held(IntervalKey{
		ChanID: intervalChanIDOf(rt, 1),
		From:   relayOf(t, rt),
		To:     createPubkey(targetNodeID),
	}))
}

// TestIntervalInFlightReleaseOnSettle tests that a settled shard gives its hold
// back, and that the settlement itself is what carries the liquidity out of the
// picture from then on.
func TestIntervalInFlightReleaseOnSettle(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	session, store := newCorridorSession(t, amt, 4)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	require.NotZero(t, store.HeldLen())

	session.ReportAttemptSuccess(0, rt)

	require.Zero(t, store.HeldLen())
	require.Empty(t, session.outstanding)
}

// TestIntervalInFlightReleaseOnFailure tests the same for a shard that failed.
func TestIntervalInFlightReleaseOnFailure(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	session, store := newCorridorSession(t, amt, 4)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	require.NotZero(t, store.HeldLen())

	failIndex := 1
	session.ReportAttemptFailure(
		0, rt, &failIndex, lnwire.NewTemporaryChannelFailure(nil),
	)

	require.Zero(t, store.HeldLen())
	require.Empty(t, session.outstanding)
}

// TestIntervalInFlightReleaseOnTeardown tests the case nothing else covers: a
// route the session handed out that never became an HTLC, so no outcome is ever
// reported for it. Without the teardown sweep its hold would sit on the node
// wide store for as long as the process lived, depressing a channel with
// nothing behind it.
func TestIntervalInFlightReleaseOnTeardown(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	session, store := newCorridorSession(t, amt, 4)

	_, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	require.NotZero(t, store.HeldLen())

	// The lifecycle exits without the route ever reaching the switch.
	session.ReleaseAttempts()

	require.Zero(t, store.HeldLen())
	require.Empty(t, session.outstanding)

	// Releasing again is harmless, since the lifecycle may well have
	// reported an outcome first.
	session.ReleaseAttempts()
	require.Zero(t, store.HeldLen())
}

// TestIntervalInFlightReconcilesAgainstLifecycle tests the net that catches a
// hold while a payment is still running. The number of HTLCs in flight comes
// from the payments database, so when we are holding more shards than that, the
// extra ones never made it out and are dropped.
func TestIntervalInFlightReconcilesAgainstLifecycle(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(400_000)
	session, store := newCorridorSession(t, amt, 8)

	// Two routes handed out, neither reported.
	_, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)
	_, err = session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 1, 0, nil)
	require.NoError(t, err)
	require.Len(t, session.outstanding, 2)

	// The lifecycle now says only one HTLC is actually in flight, so the
	// older hold is given back.
	_, err = session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 1, 0, nil)
	require.NoError(t, err)

	// One dropped by the reconciliation, one added by the request above.
	require.Len(t, session.outstanding, 2)

	// And with the payment reporting nothing in flight at all, everything
	// is given back.
	session.reconcileHolds(0)
	require.Empty(t, session.outstanding)
	require.Zero(t, store.HeldLen())
}

// TestIntervalInFlightHoldsAtPairScope tests that a hold lands under the same
// key the pricing uses. On a pair with several channels an observation cannot
// name one of them, so neither can a hold: it belongs to the pair.
func TestIntervalInFlightHoldsAtPairScope(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(100_000)
	session, store := newParallelSession(t, 2, amt)

	rt, err := session.RequestRoute(amt, lnwire.MaxMilliSatoshi, 0, 0, nil)
	require.NoError(t, err)

	var (
		relay  = createPubkey(firstRelayID)
		target = createPubkey(targetNodeID)
	)

	// Neither channel of the pair carries the hold on its own.
	for _, chanID := range []uint64{100, 101} {
		require.Zero(t, store.Held(IntervalKey{
			ChanID: chanID, From: relay, To: target,
		}))
	}

	// The pair carries it, which is the key the search prices under.
	pair := IntervalKey{From: relay, To: target}
	require.True(t, pair.IsPairScoped())
	require.Equal(t, rt.Hops[0].AmtToForward, store.Held(pair))

	// And it is given back at the same scope.
	session.ReportAttemptSuccess(0, rt)
	require.Zero(t, store.Held(pair))
	require.Zero(t, store.HeldLen())
}

// TestIntervalInFlightSharedAcrossPayments tests that the overlay is node wide.
// A second payment, with its own session, prices a corridor knowing what the
// first payment is holding on it.
func TestIntervalInFlightSharedAcrossPayments(t *testing.T) {
	t.Parallel()

	amt := lnwire.NewMSatFromSatoshis(600_000)
	first, store := newCorridorSession(t, amt, 4)

	// A second payment sharing the same node wide store.
	second, _ := newCorridorSession(t, amt, 4)
	second.store = store

	firstRoute, err := first.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)

	// The other payment, which has sent nothing itself, still steps around
	// the corridor the first one is using.
	secondRoute, err := second.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.NotEqual(
		t, relayOf(t, firstRoute), relayOf(t, secondRoute),
	)

	// Each payment gives back only its own hold.
	first.ReleaseAttempts()
	require.Equal(t, 1, store.HeldLen())

	second.ReleaseAttempts()
	require.Zero(t, store.HeldLen())
}

// intervalChanIDOf returns the channel id of the given hop of a route.
func intervalChanIDOf(rt *route.Route, hop int) uint64 {
	return rt.Hops[hop].ChannelID
}
