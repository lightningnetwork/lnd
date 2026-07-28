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
//
// THE FEE BUDGET is the one constraint that does not arrive through
// ReportAttempt. It is handed over up front, in the payment spec, because a
// real sender chooses it rather than discovering it: see
// SimPaymentSpec.FeeLimitMsat. A route whose fee would take the payment past
// that budget is refused by the runner and never sent, which costs an attempt
// and teaches nothing, so a router that wants those attempts back has to price
// its own routes before it offers them.
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

// SimBalanceRefresher is the optional half of the SimRouter contract that a
// router implements if it wants to be told that its own outbound liquidity
// changed under it.
//
// A router is built once per payment and handed the sender's local balances at
// that moment. While one payment runs at a time that snapshot stays true for
// the whole payment. It stops being true the moment the sender runs several of
// its own payments at once: a sibling's shard takes some of the same outbound
// liquidity, and the map this router is planning against still shows it.
//
// A router that does not implement this keeps the snapshot it was built with,
// which is what every router in this program does today. That is a legitimate
// design, not a bug: the balances a sender is handed at plan time are what a
// real node's path finding runs against too. The interface exists so that a
// router which wants the update can have it, and so that "the refresh did not
// help" is distinguishable from "the refresh was never delivered" — exp-016
// had to hand-write importer variants of two champions after the fact because
// nothing in the contract had ever asked for the capability.
type SimBalanceRefresher interface {
	// RefreshLocalBalances delivers the sender's current outbound
	// liquidity per channel id, net of what its own in-flight htlcs hold,
	// before each route request. Implementations must treat it as a
	// replacement for the map they were built with rather than as
	// additional evidence: it is the same measurement, taken later.
	RefreshLocalBalances(balances map[uint64]lnwire.MilliSatoshi)
}

// SimPaymentSpec describes one payment for a router to complete.
//
// Everything here is information a real sender has about its own payment
// before it sends anything, which is the rule the sealed view is built on: a
// candidate may be told what its own node knows and nothing else. A fee budget
// is squarely on that side of the line. A real sender picks the most it is
// willing to pay before it looks for a route, and lnd's own pathfinder has
// taken the number as a restriction for years.
type SimPaymentSpec struct {
	// Target is the destination node.
	Target route.Vertex

	// Amount is the total amount to deliver.
	Amount lnwire.MilliSatoshi

	// MaxParts caps the number of concurrent MPP shards the sender is
	// willing to use. 1 disables splitting.
	MaxParts uint32

	// FeeLimitMsat is the most this payment may pay in fees IN TOTAL,
	// across every shard it ends up using, not per attempt and not per
	// shard. The runner enforces it at the point it dispatches an htlc: a
	// route whose fee would take the payment's committed fees past this
	// number is refused with a SimFeeLimitFailure instead of being sent,
	// so a router that ignores the budget pays for it in attempts and in
	// failed payments rather than in a small subtraction.
	//
	// Fees already committed by settled shards count against it, so the
	// budget a router has left for its next shard is this number minus
	// what its earlier shards paid. That is exactly what lnd's own
	// lifecycle does with the same field (calcFeeBudget subtracts
	// FeesPaid), and the lnd arm here is wired to it.
	//
	// lnwire.MaxMilliSatoshi means no limit, and is what every scenario
	// file written before stage C produces. It is deliberately NOT zero:
	// zero is a real budget that forbids paying any fee at all, and a
	// router reading a zero as "unlimited" would have the sign of the
	// constraint backwards.
	FeeLimitMsat lnwire.MilliSatoshi
}

// SimNetworkView is the read-only public surface a router sees: the gossip
// graph for path queries plus the current time. It intentionally hides the
// concrete graph type so that candidate implementations cannot reach the
// hidden balances or mutate liquidity — the same information asymmetry a
// real sender faces.
//
// INBOUND FEES, and the one direction rule worth reading twice. Iterating a
// node's channels with ForEachNodeDirectedChannel yields DirectedChannel
// values whose InboundFee belongs to the node being iterated, NOT to the node
// at the other end. It is what that node charges for htlcs ARRIVING to it over
// that channel, and it is charged on the amount the node forwards onward plus
// the fee it charges for forwarding, never on the amount it received. It is
// signed, and in practice it is usually negative: a discount for inbound flow.
//
// A forwarding node's total fee is its outbound fee plus its inbound fee,
// floored at zero, so a discount larger than the outbound fee it nets against
// buys a free forward and no more. The sender pays no inbound fee to itself
// and the destination charges none, so on a route of k hops there are k-1
// inbound fees to price.
//
// A router that scores edges from a single policy per directed edge will miss
// all of this, because an inbound fee is attached to the node receiving rather
// than to the direction of flow. DirectedChannel.InPolicy carries no inbound
// fee at all here: on lnd's own cache that field describes a different node
// than DirectedChannel.InboundFee does, and this view exposes the fee in
// exactly one unambiguous place instead of reproducing that trap.
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

	// feeLimit is the payment's whole fee budget, lnwire.MaxMilliSatoshi
	// when the scenario named none.
	feeLimit lnwire.MilliSatoshi

	// feesPaid is what the shards that already went through have cost,
	// which is subtracted from the budget before the next route is
	// requested. It stands in for the MPPaymentState.FeesPaid that lnd's
	// own lifecycle reads out of the payment database, and it is
	// accumulated from the same place: the attempts that did not fail.
	feesPaid lnwire.MilliSatoshi

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
		session:  session,
		mc:       mc,
		feeLimit: spec.FeeLimitMsat,
	}, nil
}

// RequestRoute delegates to the payment session, which runs the production
// path finding (Dijkstra + probability estimator + MPP splitting).
//
// The fee limit passed down is what the budget has left after the shards that
// already went through, which is what lnd's own paymentLifecycle passes
// (calcFeeBudget over the payment's FeesPaid). Path finding prunes any partial
// path whose accumulated fee exceeds it, so the route that comes back is one
// the payment can afford and the runner's backstop has nothing to refuse.
//
// NOTE: Part of the SimRouter interface.
func (l *lndStackRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if l.terminalFailure != nil {
		return nil, l.terminalFailure
	}

	return l.session.RequestRoute(
		amt, simRemainingBudget(l.feeLimit, l.feesPaid), inFlightHtlcs,
		0, nil,
	)
}

// ReportAttempt feeds the outcome into mission control.
//
// NOTE: Part of the SimRouter interface.
func (l *lndStackRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result SimHtlcResult) error {

	if result.Failure == nil {
		// An attempt that did not fail has committed its fee, whether
		// it settled outright or is being held at the destination
		// while its siblings arrive. The runner charges the budget for
		// both, so this side has to as well.
		l.feesPaid += rt.TotalFees()

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
		FeeLimit:       spec.FeeLimitMsat,
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
