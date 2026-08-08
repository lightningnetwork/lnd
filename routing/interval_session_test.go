package routing

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Node ids used by the interval session tests, on top of the source and target
// ids the mock graph already defines.
const (
	firstRelayID  = 3
	secondRelayID = 4
)

// intervalTestCtx drives an interval session against the mock graph, standing
// in for the payment lifecycle: it asks for a route, sends it over the mock
// network, and reports the outcome back to the session the way the lifecycle
// does.
type intervalTestCtx struct {
	t *testing.T

	graph   *mockGraph
	store   *IntervalStore
	session *intervalPaymentSession
	payment *LightningPayment

	nextAttemptID uint64
}

// newIntervalTestCtx builds a context around a graph the caller has already
// populated.
func newIntervalTestCtx(t *testing.T, graph *mockGraph,
	amt lnwire.MilliSatoshi, maxParts uint32,
	splittable bool) *intervalTestCtx {

	t.Helper()

	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		FeeLimit:       lnwire.MaxMilliSatoshi,
		Target:         graph.nodes[createPubkey(targetNodeID)].pubkey,
		Amount:         amt,
		CltvLimit:      math.MaxUint32,
		MaxParts:       maxParts,
	}

	// A payment can only be split when the receiver can be told what the
	// parts add up to, which needs both a payment address and a receiver
	// that understands MPP.
	if splittable {
		var paymentAddr [32]byte
		payment.PaymentAddr = fn.Some(paymentAddr)
		payment.DestFeatures = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
				lnwire.PaymentAddrOptional,
				lnwire.MPPOptional,
			), lnwire.Features,
		)
	} else {
		payment.DestFeatures = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
			), lnwire.Features,
		)
	}

	var paymentHash [32]byte
	require.NoError(t, payment.SetPaymentHash(paymentHash))

	getBandwidthHints := func(_ Graph) (bandwidthHints, error) {
		hints := map[uint64]lnwire.MilliSatoshi{}
		for _, ch := range graph.source.channels {
			hints[ch.id] = ch.balance
		}

		return &mockBandwidthHints{hints: hints}, nil
	}

	store := NewIntervalStore(0)
	cfg := DefaultIntervalConfig()

	// The mock graph's channels are small, so shrink the minimum shard size
	// to let the ladder cut them.
	cfg.MinShardAmt = lnwire.NewMSatFromSatoshis(1000)

	session, err := newIntervalPaymentSession(
		payment, graph.source.pubkey, getBandwidthHints, graph, store,
		cfg,
	)
	require.NoError(t, err)

	return &intervalTestCtx{
		t:       t,
		graph:   graph,
		store:   store,
		session: session,
		payment: payment,
	}
}

// newIntervalTestGraph builds a graph with a source, a target and the given
// number of relays, each relay carrying a channel from the source and a channel
// to the target.
func newIntervalTestGraph(t *testing.T, relays []byte,
	capacity btcutil.Amount) *mockGraph {

	t.Helper()

	graph := newMockGraph(t)

	source := newMockNode(sourceNodeID)
	target := newMockNode(targetNodeID)
	graph.addNode(source)
	graph.addNode(target)
	graph.source = source

	var chanID uint64
	for _, relay := range relays {
		graph.addNode(newMockNode(relay))

		chanID++
		graph.addChannel(chanID, sourceNodeID, relay, capacity)

		chanID++
		graph.addChannel(chanID, relay, targetNodeID, capacity)
	}

	return graph
}

// setBalance overrides the balance a node holds on its channel to a peer, which
// is how these tests arrange for a forward to fail.
func (c *intervalTestCtx) setBalance(node, peer byte,
	balance lnwire.MilliSatoshi) {

	c.t.Helper()

	channel, ok := c.graph.nodes[createPubkey(node)].
		channels[createPubkey(peer)]
	require.True(c.t, ok, "channel between %v and %v not found", node, peer)

	channel.balance = balance
}

// attempt asks the session for a route for the given remaining amount, sends it
// over the mock network, and reports the outcome back. It returns the route and
// whether it settled.
func (c *intervalTestCtx) attempt(remaining lnwire.MilliSatoshi,
	inFlight uint32) (*route.Route, bool, error) {

	c.t.Helper()

	rt, err := c.session.RequestRoute(
		remaining, lnwire.MaxMilliSatoshi, inFlight, 0, nil,
	)
	if err != nil {
		return nil, false, err
	}

	attemptID := c.nextAttemptID
	c.nextAttemptID++

	result, err := c.graph.sendHtlc(rt)
	require.NoError(c.t, err)

	if result.failure == nil {
		c.session.ReportAttemptSuccess(attemptID, rt)

		return rt, true, nil
	}

	c.session.ReportAttemptFailure(
		attemptID, rt, getNodeIndex(rt, result.failureSource),
		result.failure,
	)

	return rt, false, nil
}

// TestIntervalSessionFindsRoute tests that the session returns a usable route
// over a graph where one exists.
func TestIntervalSessionFindsRoute(t *testing.T) {
	t.Parallel()

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, 100_000)
	ctx := newIntervalTestCtx(
		t, graph, lnwire.NewMSatFromSatoshis(10_000), 1, false,
	)

	rt, settled, err := ctx.attempt(
		lnwire.NewMSatFromSatoshis(10_000), 0,
	)
	require.NoError(t, err)
	require.True(t, settled)

	// The only path is source, relay, target.
	require.Len(t, rt.Hops, 2)
	require.Equal(
		t, createPubkey(firstRelayID), rt.Hops[0].PubKeyBytes,
	)
	require.Equal(t, createPubkey(targetNodeID), rt.Hops[1].PubKeyBytes)
	require.Equal(
		t, lnwire.NewMSatFromSatoshis(10_000), rt.ReceiverAmt(),
	)

	// A settled route leaves the belief store holding what it moved, in
	// both directions.
	forward := IntervalKey{
		ChanID: 2,
		From:   createPubkey(firstRelayID),
		To:     createPubkey(targetNodeID),
	}
	capacity := lnwire.NewMSatFromSatoshis(100_000)

	require.True(t, ctx.store.Get(forward, capacity).Known)
	require.True(t, ctx.store.Get(forward.Reverse(), capacity).Known)
	require.GreaterOrEqual(
		t, ctx.store.Get(forward.Reverse(), capacity).LowerOK,
		lnwire.NewMSatFromSatoshis(10_000),
	)
}

// TestIntervalSessionBoundsFailedChannel tests what a failure buys. The amount
// that failed becomes impossible rather than merely expensive, so the payment
// stops rather than retrying a route it now knows cannot work, and the bound
// outlives the payment: it is a smaller amount that the next one is free to
// try, not a channel that has been blacklisted.
func TestIntervalSessionBoundsFailedChannel(t *testing.T) {
	t.Parallel()

	const capacitySat = 100_000

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, capacitySat)

	amt := lnwire.NewMSatFromSatoshis(40_000)
	ctx := newIntervalTestCtx(t, graph, amt, 1, false)

	// Starve the relay's channel to the target, so that it can forward a
	// small amount but not the one we are about to send.
	ctx.setBalance(
		firstRelayID, targetNodeID,
		lnwire.NewMSatFromSatoshis(5_000),
	)

	rt, settled, err := ctx.attempt(amt, 0)
	require.NoError(t, err)
	require.False(t, settled)
	require.Equal(t, createPubkey(firstRelayID), rt.Hops[0].PubKeyBytes)

	// The failure left a bound in the store rather than a penalty that will
	// fade, and the bound says the amount is impossible.
	failed := IntervalKey{
		ChanID: 2,
		From:   createPubkey(firstRelayID),
		To:     createPubkey(targetNodeID),
	}
	capacity := lnwire.NewMSatFromSatoshis(capacitySat)

	interval := ctx.store.Get(failed, capacity)
	require.Equal(t, amt, interval.UpperFail)
	require.Zero(t, ctx.store.Probability(failed, amt, capacity))

	// The payment cannot be split, so with its only route ruled out at this
	// amount there is nothing left for it to try.
	_, err = ctx.session.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.ErrorIs(t, err, errNoPathFound)

	// A bound is not a blacklist. A later payment reading the same store
	// finds the channel perfectly usable below the amount that failed.
	small := lnwire.NewMSatFromSatoshis(1_000)
	require.Greater(t, ctx.store.Probability(failed, small, capacity), 0.0)

	next := newIntervalTestCtx(t, graph, small, 1, false)
	next.store = ctx.store
	next.session.store = ctx.store

	rt, settled, err = next.attempt(small, 0)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, small, rt.ReceiverAmt())
}

// TestIntervalSessionSplits tests that the session cuts a payment that no
// single channel can carry, choosing the shard size itself rather than halving
// its way down to one.
func TestIntervalSessionSplits(t *testing.T) {
	t.Parallel()

	const capacitySat = 100_000

	graph := newIntervalTestGraph(
		t, []byte{firstRelayID, secondRelayID}, capacitySat,
	)

	// Each of our own channels holds half its capacity, so no single path
	// can carry an amount above that, but the two together can.
	amt := lnwire.NewMSatFromSatoshis(70_000)
	ctx := newIntervalTestCtx(t, graph, amt, 3, true)

	remaining := amt
	inFlight := uint32(0)

	var shards []lnwire.MilliSatoshi
	for remaining > 0 {
		require.Less(t, len(shards), 5, "too many shards")

		rt, settled, err := ctx.attempt(remaining, inFlight)
		require.NoError(t, err)
		require.True(t, settled)

		shards = append(shards, rt.ReceiverAmt())
		remaining -= rt.ReceiverAmt()
		inFlight++
	}

	// The payment had to be split, and every shard was smaller than the
	// whole.
	require.Greater(t, len(shards), 1)

	var total lnwire.MilliSatoshi
	for _, shard := range shards {
		require.Less(t, shard, amt)
		total += shard
	}
	require.Equal(t, amt, total)
}

// TestIntervalSessionRefusesToSplit tests that a payment the receiver could not
// reassemble is never cut, no matter how many parts it allows.
func TestIntervalSessionRefusesToSplit(t *testing.T) {
	t.Parallel()

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, 100_000)

	// This amount is above what our own channel holds, so the only way to
	// deliver it would be to split, which this payment cannot do.
	amt := lnwire.NewMSatFromSatoshis(70_000)
	ctx := newIntervalTestCtx(t, graph, amt, 10, false)

	_, err := ctx.session.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.ErrorIs(t, err, errInsufficientBalance)
}

// TestIntervalSessionAttemptLimit tests that a session which keeps finding
// routes still gives up eventually.
func TestIntervalSessionAttemptLimit(t *testing.T) {
	t.Parallel()

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, 100_000)

	amt := lnwire.NewMSatFromSatoshis(10_000)
	ctx := newIntervalTestCtx(t, graph, amt, 1, false)
	ctx.session.cfg.AttemptLimit = 3

	for i := 0; i < 3; i++ {
		_, err := ctx.session.RequestRoute(
			amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
		)
		require.NoError(t, err)
	}

	_, err := ctx.session.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.ErrorIs(t, err, errNoPathFound)
}

// TestIntervalShardAmounts tests the ladder of candidate shard sizes: it must
// stay inside the bounds the payment sets, and it must react to the amounts the
// payment has already proven do not fit.
func TestIntervalShardAmounts(t *testing.T) {
	t.Parallel()

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, 100_000)
	amt := lnwire.NewMSatFromSatoshis(100_000)
	ctx := newIntervalTestCtx(t, graph, amt, 4, true)

	session := ctx.session

	// With a single part left, the only candidate is the whole amount.
	require.Equal(
		t, []lnwire.MilliSatoshi{amt},
		session.shardAmounts(amt, amt, 1),
	)

	// With four parts left, every candidate has to be large enough that
	// four of them could still deliver the amount, and none may exceed it.
	minimum := intervalCeilDiv(amt, 4)
	amounts := session.shardAmounts(amt, minimum, 4)

	require.Contains(t, amounts, amt)
	require.Contains(t, amounts, minimum)
	for _, shard := range amounts {
		require.GreaterOrEqual(t, shard, minimum)
		require.LessOrEqual(t, shard, amt)
	}

	// The ladder does not cut below the minimum shard size, other than to
	// carry the whole remaining amount in one go.
	small := session.cfg.MinShardAmt / 2
	require.Equal(
		t, []lnwire.MilliSatoshi{small},
		session.shardAmounts(small, small/4, 4),
	)

	// An amount this payment has proven does not fit puts shard sizes just
	// under it into play, which the ladder would otherwise never consider.
	failedAt := amt*3/4 + 1
	session.failedAt[IntervalKey{ChanID: 1}] = failedAt

	withEvidence := session.shardAmounts(amt, minimum, 4)
	require.Contains(t, withEvidence, (failedAt-1)/2)
	require.NotContains(t, amounts, (failedAt-1)/2)

	// Every rung costs a full search, so the ladder is capped. The rungs
	// that survive the cap are the ones enumerated first, which are the
	// whole amount, the smallest usable shard, and the sizes the payment's
	// own failures put into play.
	session.cfg.MaxLadderRungs = 3
	capped := session.shardAmounts(amt, minimum, 4)

	require.Len(t, capped, 3)
	require.Equal(
		t, []lnwire.MilliSatoshi{amt, minimum, (failedAt - 1) / 2},
		capped,
	)
}

// TestIntervalSessionSourceFallback tests that the payment shapes the interval
// router does not handle are served by the stock session instead, so that
// turning the router on cannot make a payment unroutable.
func TestIntervalSessionSourceFallback(t *testing.T) {
	t.Parallel()

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, 100_000)

	stock := &SessionSource{
		GraphSessionFactory: graph,
		SourceNode: &models.Node{
			PubKeyBytes: graph.source.pubkey,
		},
	}
	source := NewIntervalSessionSource(
		stock, NewIntervalStore(0), IntervalConfig{},
	)

	var paymentAddr [32]byte
	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		Target:         createPubkey(targetNodeID),
		PaymentAddr:    fn.Some(paymentAddr),
		Amount:         1000,
		CltvLimit:      math.MaxUint32,
		MaxParts:       1,
	}
	require.NoError(t, payment.SetPaymentHash([32]byte{}))

	// An ordinary payment is served by the interval session.
	session, err := source.NewPaymentSession(payment, fn.None[tlv.Blob](),
		fn.None[htlcswitch.AuxTrafficShaper]())
	require.NoError(t, err)
	require.IsType(t, &intervalPaymentSession{}, session)

	// A payment to a blinded path falls back to the stock one, because the
	// interval model has no directed channel to key its beliefs on inside a
	// blinded path. The fallback has to be graceful: a payment lnd can route
	// today must not become unroutable because this router is switched on.
	//
	// Route hints and a blinded path are mutually exclusive, so drop the
	// hints the way a real blinded payment would arrive.
	payment.RouteHints = nil
	payment.BlindedPathSet = newTestBlindedPathSet(t)

	session, err = source.NewPaymentSession(payment, fn.None[tlv.Blob](),
		fn.None[htlcswitch.AuxTrafficShaper]())
	require.NoError(t, err)
	require.IsType(t, &paymentSession{}, session)

	// The fallback is transparent: it is exactly the session the stock
	// source would have handed out on its own.
	stockSession, err := stock.NewPaymentSession(
		payment, fn.None[tlv.Blob](),
		fn.None[htlcswitch.AuxTrafficShaper](),
	)
	require.NoError(t, err)
	require.IsType(t, stockSession, session)

	// A session that came from the fallback is the stock one all the way
	// through, so it never reports attempts to a belief store that has no
	// way to key them.
	_, reports := session.(PaymentResultReporter)
	require.False(t, reports)

	// The empty session is the stock one either way, since it holds no
	// routing at all.
	require.IsType(t, &paymentSession{}, source.NewPaymentSessionEmpty())
}

// recordingSession is a payment session that only records what the payment
// lifecycle tells it, so that the seam itself can be tested apart from the
// interval router that needed it.
type recordingSession struct {
	PaymentSession

	successes []uint64
	failures  []uint64
	released  int
}

// ReportAttemptSuccess records a settled attempt.
//
// NOTE: Part of the PaymentResultReporter interface.
func (r *recordingSession) ReportAttemptSuccess(attemptID uint64,
	_ *route.Route) {

	r.successes = append(r.successes, attemptID)
}

// ReportAttemptFailure records a failed attempt.
//
// NOTE: Part of the PaymentResultReporter interface.
func (r *recordingSession) ReportAttemptFailure(attemptID uint64,
	_ *route.Route, _ *int, _ lnwire.FailureMessage) {

	r.failures = append(r.failures, attemptID)
}

// ReleaseAttempts records that the lifecycle told the session it was done.
//
// NOTE: Part of the PaymentResultReporter interface.
func (r *recordingSession) ReleaseAttempts() {
	r.released++
}

// TestLifecycleReportsToSession tests that the payment lifecycle hands an
// attempt outcome to a session that asked for it, on both the settle and the
// failure path, and that it does so alongside mission control rather than
// instead of it.
func TestLifecycleReportsToSession(t *testing.T) {
	t.Parallel()

	p, m := newTestPaymentLifecycle(t)

	session := &recordingSession{PaymentSession: m.paySession}
	p.paySession = session

	preimage := lntypes.Preimage{1}
	attempt := makeSettledAttempt(t, 10_000, preimage)

	m.clock.On("Now").Return(time.Now())

	// A settled attempt is reported to both mission control and the
	// session.
	m.missionControl.On("ReportPaymentSuccess",
		attempt.AttemptID, &attempt.Route,
	).Return(nil).Once()
	m.control.On("SettleAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(attempt, nil).Once()

	_, err := p.handleAttemptResult(
		t.Context(), attempt, &htlcswitch.PaymentResult{
			Preimage: preimage,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []uint64{attempt.AttemptID}, session.successes)
	require.Empty(t, session.failures)

	// So is a failed one. An unreadable failure reaches mission control
	// with neither a source nor a message, and the session hears about it
	// on the same terms.
	m.missionControl.On("ReportPaymentFail",
		attempt.AttemptID, &attempt.Route, mock.Anything, mock.Anything,
	).Return(nil, nil).Once()
	m.shardTracker.On("CancelShard", attempt.AttemptID).Return(nil).Once()
	m.control.On("FailAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(attempt, nil).Once()

	_, err = p.handleSwitchErr(
		t.Context(), attempt, htlcswitch.ErrUnreadableFailureMessage,
	)
	require.NoError(t, err)
	require.Equal(t, []uint64{attempt.AttemptID}, session.failures)
}

// newTestBlindedPathSet builds a blinded path set that passes validation, so
// that the fallback is exercised on a payment lnd would really accept rather
// than on an empty struct.
func newTestBlindedPathSet(t *testing.T) *BlindedPaymentPathSet {
	t.Helper()

	_, introPoint := btcec.PrivKeyFromBytes([]byte{1})
	_, blindedPoint := btcec.PrivKeyFromBytes([]byte{5})

	payment := &BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: introPoint,
			BlindingPoint:     blindedPoint,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					BlindedNodePub: introPoint,
					CipherText: bytes.Repeat(
						[]byte{1}, 100,
					),
				},
			},
		},
		BaseFee:             1000,
		ProportionalFeeRate: 500,
		CltvExpiryDelta:     140,
		HtlcMinimum:         100,
		HtlcMaximum:         100_000_000,
		Features:            lnwire.EmptyFeatureVector(),
	}

	set, err := NewBlindedPaymentPathSet([]*BlindedPayment{payment})
	require.NoError(t, err)

	return set
}

// TestStockSessionReportsNothing tests that with the interval router switched
// off, the seam it needed in the payment lifecycle is inert. The stock session
// does not implement the reporting interface, so the lifecycle's type assertion
// never fires and every attempt outcome goes to mission control and nowhere
// else, exactly as it did before.
func TestStockSessionReportsNothing(t *testing.T) {
	t.Parallel()

	var session PaymentSession = &paymentSession{}

	_, reports := session.(PaymentResultReporter)
	require.False(t, reports, "the stock session must stay inert")

	// The interval session is the one that asked for the seam.
	session = &intervalPaymentSession{}
	_, reports = session.(PaymentResultReporter)
	require.True(t, reports)
}
