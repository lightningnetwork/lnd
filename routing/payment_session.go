package routing

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlockPadding is used to increment the finalCltvDelta value for the last hop
// to prevent an HTLC being failed if some blocks are mined while it's in-flight.
const BlockPadding uint16 = 3

var (
	// errPreBuiltRouteTried is returned when the single pre-built route
	// failed and there is nothing more we can do.
	errPrebuiltRouteTried = errors.New("pre-built route already tried")

	// errShutdown is returned if the payment session has exited.
	errShutdown = errors.New("payment session shutting down")
)

// RouteResult is a result delivered by the consumer the payment sesssion, in
// response to a RouteIntent. It wraps the result of sending along the Route,
// and is used to deliver the result back to users of AddRoute.
type RouteResult struct {
	// Preimage is the preimage in case of a successful payment. only set
	// if Err != nil.
	Preimage lntypes.Preimage

	// Err is any encountered error from sending along the route.
	Err error
}

// RouteIntent is a route produced by the payment session, and a channel used
// to notify the payment session about the result of using this route.
type RouteIntent struct {
	// Route is the route in response to the RequestRoute call.
	Route *route.Route

	// ResultChan is a channel that should be used by the requester to
	// notify the payment session about the result of using the route.
	ResultChan chan *RouteResult
}

// PaymentSession is used during SendPayment and SendToRoute attempts to
// provide routes to attempt.
type PaymentSession interface {
	// RequestRoutes intructs the PaymentSession to find more routes that
	// in total send up to amt. Two channels are returned: one where a new
	// route to try will be delived, and one where any error encountered
	// will be delivered.
	//
	// NOTE: should always be used from a single thread to ensure the
	// produced route will be updated.
	RequestRoute(amt lnwire.MilliSatoshi, payment *LightningPayment,
		height uint32, finalCltvDelta uint16) (chan *RouteIntent,
		chan error)

	// Cancel instructs the PaymentSession to quit any sub goroutines. It
	// cannot be used after this has been called.
	Cancel()
}

// paymentSessionBuilder is an implementation of PaymentSession that can be
// used in combination with SendToRoute. It simply takes routes provided to it
// via its AddRoute method, and feeds them to the requester. If no route has
// been added after a request has been made, it will return an error after a
// timeout.
type paymentSessionBuilder struct {
	// timeout is the maximum time to wait for new routes to be added
	// before returning an error in response to a route request.
	timeout time.Duration

	// reqSignal is a channel where we signal that a new RequestRoute call
	// has been made. This will instruct any goroutines from previous calls
	// to exit.
	reqSignal    chan struct{}
	reqSignalMtx sync.Mutex

	// reqWg is a waitgroup that is counting the active goroutines from
	// calls to RequestRoute.
	reqWg sync.WaitGroup

	// addRouteSignal is a channel used to signal that a new route has been
	// added to the route queue.
	addRouteSignal chan struct{}

	// routeQueue is a queue of routes added to the payment session.
	routeQueue    []*RouteIntent
	routeQueueMtx sync.Mutex

	quit chan struct{}
}

// Cancel instructs the PaymentSession to quit any sub goroutines. It cannot be
// used after this has been called.
//
// NOTE: Part of the PaymentSession interface.
func (p *paymentSessionBuilder) Cancel() {
	close(p.quit)

	p.reqWg.Wait()
}

// AddRoute appends the given route to queue of routes produced by the
// PaymentSession, and returns the result from using this route as part of the
// payment.
func (p *paymentSessionBuilder) AddRoute(r *route.Route) (
	lntypes.Preimage, error) {

	result := make(chan *RouteResult, 1)
	shard := &RouteIntent{
		Route:      r,
		ResultChan: result,
	}

	// Add the route to the queue.
	p.routeQueueMtx.Lock()
	p.routeQueue = append(p.routeQueue, shard)
	p.routeQueueMtx.Unlock()

	// We'll signal that a new route was added. We don't block in case it
	// has already been signaled.
	select {
	case p.addRouteSignal <- struct{}{}:
	default:
	}

	// We'll wait here until a result is available, or a we are instructed
	// to quit.
	select {
	case res := <-result:
		return res.Preimage, res.Err
	case <-p.quit:
		return lntypes.Preimage{}, errShutdown
	}
}

// RequestRoutes intructs the PaymentSession to find more routes that in total
// send up to amt. Two channels are returned: one where a new route to try will
// be delived, and one where any error encountered will be delivered.
//
// NOTE: should always be used from a single thread to ensure the produced
// route will be updated. Only the last call to RequestRoute is guaranteed to
// produce a result.
//
// NOTE: Part of the PaymentSession interface.
func (p *paymentSessionBuilder) RequestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment, height uint32, finalCltvDelta uint16) (
	chan *RouteIntent, chan error) {

	errChan := make(chan error, 1)
	routes := make(chan *RouteIntent)

	// We'll start by signalling any other previous RequestRoute call that
	// it can exit.
	p.reqSignalMtx.Lock()
	close(p.reqSignal)

	// Wait for the other goroutines to have properly exited.
	p.reqWg.Wait()

	// Set up a new signal channel now that the previous one has been read
	// by all relevant goroutines.
	p.reqSignal = make(chan struct{})
	p.reqSignalMtx.Unlock()

	p.reqWg.Add(1)
	go func() {
		defer p.reqWg.Done()

		// Check whether a route is available.
		p.routeQueueMtx.Lock()
		for len(p.routeQueue) == 0 {
			p.routeQueueMtx.Unlock()

			// If no route is available, we'll wait for a signal to
			// proceed.
			select {

			// A new route has been added to the queue.
			case <-p.addRouteSignal:
				p.routeQueueMtx.Lock()

			// Another RequestRoute call happened, exit.
			case <-p.reqSignal:
				return

			// Timeout, send error.
			case <-time.After(p.timeout):
				errChan <- fmt.Errorf("pay session timeout")
				return

			// PaymestSession has exited.
			case <-p.quit:
				errChan <- errShutdown
				return
			}
		}

		// Get the route first in the queue.
		r := p.routeQueue[0]
		p.routeQueueMtx.Unlock()

		// Now we'll attempt to send this route to the caller. Since
		// the routes channel is unbuffered, we know that if we are
		// able to send it, the caller has successfully gotten it, and
		// we can remove it from the queue.
		select {

		// Route was read by caller, pop it.
		case routes <- r:
			p.routeQueueMtx.Lock()
			p.routeQueue[0] = nil
			p.routeQueue = p.routeQueue[1:]
			p.routeQueueMtx.Unlock()

		// Another RequestRoute call happened, exit.
		case <-p.reqSignal:
			return

		// PaymentSession is exiting.
		case <-p.quit:
			errChan <- errShutdown
			return
		}
	}()

	return routes, errChan
}

// paymentSession is used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// MissionControl. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type paymentSession struct {
	additionalEdges map[route.Vertex][]*channeldb.ChannelEdgePolicy

	getBandwidthHints func() (map[uint64]lnwire.MilliSatoshi, error)

	sessionSource *SessionSource

	pathFinder pathFinder
	wg         sync.WaitGroup
}

// Cancel instructs the PaymentSession to quit any sub goroutines. It cannot be
// used after this has been called.
//
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) Cancel() {
	p.wg.Wait()
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) RequestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (chan *RouteIntent, chan error) {

	respChan := make(chan *RouteIntent, 1)
	errChan := make(chan error, 1)

	p.wg.Add(1)
	go p.requestRoute(
		amt, payment, height, finalCltvDelta, respChan, errChan,
	)

	return respChan, errChan
}

func (p *paymentSession) requestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment,
	height uint32, finalCltvDelta uint16, respChan chan *RouteIntent,
	errChan chan error) {

	defer p.wg.Done()

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in-flight.
	finalCltvDelta += BlockPadding

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	cltvLimit := payment.CltvLimit - uint32(finalCltvDelta)

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// MissionControl.
	ss := p.sessionSource

	restrictions := &RestrictParams{
		ProbabilitySource: ss.MissionControl.GetProbability,
		FeeLimit:          payment.FeeLimit,
		OutgoingChannelID: payment.OutgoingChannelID,
		LastHop:           payment.LastHop,
		CltvLimit:         cltvLimit,
		DestCustomRecords: payment.DestCustomRecords,
		DestFeatures:      payment.DestFeatures,
		PaymentAddr:       payment.PaymentAddr,
	}

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough bandwidth
	// to carry the payment. New bandwidth hints are queried for every new
	// path finding attempt, because concurrent payments may change
	// balances.
	bandwidthHints, err := p.getBandwidthHints()
	if err != nil {
		errChan <- err
		return
	}

	finalHtlcExpiry := int32(height) + int32(finalCltvDelta)

	path, err := p.pathFinder(
		&graphParams{
			graph:           ss.Graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  bandwidthHints,
		},
		restrictions, &ss.PathFindingConfig,
		ss.SelfNode.PubKeyBytes, payment.Target,
		amt, finalHtlcExpiry,
	)
	if err != nil {
		errChan <- err
		return

	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := route.Vertex(ss.SelfNode.PubKeyBytes)
	rt, err := newRoute(
		sourceVertex, path, height,
		finalHopParams{
			amt:         amt,
			cltvDelta:   finalCltvDelta,
			records:     payment.DestCustomRecords,
			paymentAddr: payment.PaymentAddr,
		},
	)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		errChan <- err
		return

	}

	s := &RouteIntent{
		Route: rt,

		// The result is ignored by the PaymentSession, but we'll make
		// a buffered channel to allow the caller to send a result
		// without blocking.
		ResultChan: make(chan *RouteResult, 1),
	}

	respChan <- s
}
