package routing

import (
	"context"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// simMaxCltvLimit disables the cltv limit in simulated payments.
const simMaxCltvLimit = math.MaxUint32

// SimRouter is the paradigm-free routing strategy interface. A router owns
// route selection end to end: which paths to try, how to split, what to
// learn from failures. It deliberately does NOT assume Dijkstra, mission
// control or any probability model — those are implementation details of
// the default lnd-stack router. Optimization candidates implement this
// interface to propose entirely new routing algorithms, competing against
// the lnd stack on identical scenarios.
type SimRouter interface {
	// RequestRoute returns the next route to attempt, delivering at most
	// amt to the target. Returning an error is terminal for the payment:
	// the router has no more routes to suggest.
	RequestRoute(amt lnwire.MilliSatoshi,
		inFlightHtlcs uint32) (*route.Route, error)

	// ReportAttempt informs the router of the outcome of a route it
	// suggested, before the next RequestRoute call. This is the only
	// learning signal a router receives, mirroring what a real sender
	// observes on the wire.
	ReportAttempt(attemptID uint64, rt *route.Route,
		result SimHtlcResult) error
}

// SimPaymentSpec describes one payment for a router to complete.
type SimPaymentSpec struct {
	// Target is the destination node.
	Target route.Vertex

	// Amount is the total amount to deliver.
	Amount lnwire.MilliSatoshi

	// MaxParts caps the number of concurrent MPP shards the sender is
	// willing to use. 1 disables splitting.
	MaxParts uint32
}

// SimNetworkView is the read-only public surface a router sees: the gossip
// graph for path queries plus the current time. It intentionally hides the
// concrete graph type so that candidate implementations cannot reach the
// hidden balances or mutate liquidity — the same information asymmetry a
// real sender faces.
type SimNetworkView interface {
	Graph
	GraphSessionFactory

	// Now returns the current time as the router experiences it. Under a
	// virtual clock this advances between payments and attempts, and
	// hidden liquidity drifts with it when background traffic is enabled.
	Now() time.Time
}

// simGossipView wraps a SimGraph, exposing only the SimNetworkView surface.
type simGossipView struct {
	g *SimGraph

	// now is the runner's time source; nil falls back to the wall clock.
	now func() time.Time
}

// Now returns the current simulation time.
//
// NOTE: Part of the SimNetworkView interface.
func (v *simGossipView) Now() time.Time {
	if v.now == nil {
		return time.Now()
	}

	return v.now()
}

func (v *simGossipView) ForEachNodeDirectedChannel(ctx context.Context,
	nodePub route.Vertex, cb func(channel *graphdb.DirectedChannel) error,
	reset func()) error {

	return v.g.ForEachNodeDirectedChannel(ctx, nodePub, cb, reset)
}

func (v *simGossipView) FetchNodeFeatures(ctx context.Context,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	return v.g.FetchNodeFeatures(ctx, nodePub)
}

func (v *simGossipView) GraphSession(_ context.Context,
	cb func(graph graphdb.NodeTraverser) error, _ func()) error {

	// Pass the sealed view itself, never the underlying graph: handing
	// the callback the concrete *SimGraph would let a candidate router
	// type-assert its way to the hidden balances and liquidity
	// mutators, defeating the sandbox entirely.
	return cb(v)
}

// SimRouterFactory builds a router bound to a network view and a source
// node, called once per payment. The view is the public gossip graph (no
// hidden balances); localBalances exposes the sender's own outbound
// liquidity per channel id, which a real node always knows exactly.
type SimRouterFactory func(view SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *SimPaymentSpec) (SimRouter, error)

// lndStackRouter adapts lnd's production payment session + mission control
// stack to the SimRouter interface. It is both the default router and the
// baseline any candidate algorithm must beat.
type lndStackRouter struct {
	session *paymentSession
	mc      *MissionControl

	// terminalFailure latches a payment-level failure reported by
	// mission control (e.g. a failure at the final node), surfaced on
	// the next RequestRoute call.
	terminalFailure error
}

// newLndStackRouter builds the baseline router from the given tunables. The
// mission control instance persists across payments of a scenario batch,
// carrying learned pair history exactly like a long-running node.
func newLndStackRouter(view SimNetworkView, mc *MissionControl,
	params *SimParams, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *SimPaymentSpec) (SimRouter, error) {

	payment, err := newSimLightningPayment(spec)
	if err != nil {
		return nil, err
	}

	getBandwidthHints := func(_ Graph) (bandwidthHints, error) {
		return &simBandwidthHints{balances: localBalances}, nil
	}

	session, err := newPaymentSession(
		payment, source, getBandwidthHints, view, mc,
		params.pathFindingConfig(),
	)
	if err != nil {
		return nil, err
	}

	return &lndStackRouter{
		session: session,
		mc:      mc,
	}, nil
}

// RequestRoute delegates to the payment session, which runs the production
// path finding (Dijkstra + probability estimator + MPP splitting).
//
// NOTE: Part of the SimRouter interface.
func (l *lndStackRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if l.terminalFailure != nil {
		return nil, l.terminalFailure
	}

	return l.session.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, inFlightHtlcs, 0, nil,
	)
}

// ReportAttempt feeds the outcome into mission control.
//
// NOTE: Part of the SimRouter interface.
func (l *lndStackRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result SimHtlcResult) error {

	if result.Failure == nil {
		return l.mc.ReportPaymentSuccess(attemptID, rt)
	}

	// An unattributed failure is reported the way lnd's own switch reports
	// an onion error it could not read: no source index and no failure
	// message. Mission control already recognizes that shape: a nil source
	// index makes newPaymentFailure drop the message and processFail
	// dispatch to processPaymentOutcomeUnknown, which penalizes the whole
	// route because any hop on it could be the guilty one. Nothing here
	// invents a policy for the degraded case; it hands lnd the input its
	// production code was written for.
	failure := result.Failure
	if _, unreadable := failure.(SimUnknownFailure); unreadable {
		failure = nil
	}

	// A non-nil final result means mission control considers the payment
	// terminally failed. Latch it so that the next RequestRoute call
	// fails, ending the payment loop.
	finalResult, err := l.mc.ReportPaymentFail(
		attemptID, rt,
		getNodeIndexSim(rt, result.FailureSource), failure,
	)
	if err != nil {
		return err
	}

	if finalResult != nil {
		l.terminalFailure = finalResult
	}

	return nil
}

// newSimLightningPayment constructs the LightningPayment describing one
// simulated payment.
func newSimLightningPayment(spec *SimPaymentSpec) (*LightningPayment, error) {
	var paymentAddr [32]byte
	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		FeeLimit:       lnwire.MaxMilliSatoshi,
		Target:         spec.Target,
		PaymentAddr:    fn.Some(paymentAddr),
		DestFeatures: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadRequired,
				lnwire.PaymentAddrOptional,
				lnwire.MPPOptional,
			),
			lnwire.Features,
		),
		Amount:    spec.Amount,
		CltvLimit: simMaxCltvLimit,
		MaxParts:  spec.MaxParts,
	}

	var paymentHash [32]byte
	if err := payment.SetPaymentHash(paymentHash); err != nil {
		return nil, err
	}

	return payment, nil
}
