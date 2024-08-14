package peer

import (
	"fmt"
	"maps"
	"sync"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrDuplicateEndpoint is returned when an endpoint is registered with
	// a name that already exists.
	ErrDuplicateEndpoint = fmt.Errorf("endpoint already registered")

	// ErrUnableToRouteMsg is returned when a message is unable to be
	// routed to any endpoints.
	ErrUnableToRouteMsg = fmt.Errorf("unable to route message")
)

// EndPointName is the name of a given endpoint. This MUST be unique across all
// registered endpoints.
type EndPointName = string

// MsgEndpoint is an interface that represents a message endpoint, or the
// sub-system that will handle processing an incoming wire message.
type MsgEndpoint interface {
	// Name returns the name of this endpoint. This MUST be unique across
	// all registered endpoints.
	Name() EndPointName

	// CanHandle returns true if the target message can be routed to this
	// endpoint.
	CanHandle(msg lnwire.Message) bool

	// SendMessage handles the target message, and returns true if the
	// message was able to be processed.
	SendMessage(msg lnwire.Message) bool
}

// MsgRouter is an interface that represents a message router, which is generic
// sub-system capable of routing any incoming wire message to a set of
// registered endpoints.
//
// TODO(roasbeef): move to diff sub-system?
type MsgRouter interface {
	// RegisterEndpoint registers a new endpoint with the router. If a
	// duplicate endpoint exists, an error is returned.
	RegisterEndpoint(MsgEndpoint) error

	// UnregisterEndpoint unregisters the target endpoint from the router.
	UnregisterEndpoint(EndPointName) error

	// RouteMsg attempts to route the target message to a registered
	// endpoint. If ANY endpoint could handle the message, then nil is
	// returned. Otherwise, ErrUnableToRouteMsg is returned.
	RouteMsg(lnwire.Message) error

	// Start starts the peer message router.
	Start()

	// Stop stops the peer message router.
	Stop()
}

// sendQuery sends a query to the main event loop, and returns the response.
func sendQuery[Q any, R any](sendChan chan fn.Req[Q, R], queryArg Q,
	quit chan struct{}) fn.Result[R] {

	query, respChan := fn.NewReq[Q, R](queryArg)

	if !fn.SendOrQuit(sendChan, query, quit) {
		return fn.Errf[R]("router shutting down")
	}

	return fn.NewResult(fn.RecvResp(respChan, nil, quit))
}

// sendQueryErr is a helper function based on sendQuery that can be used when
// the query only needs an error response.
func sendQueryErr[Q any](sendChan chan fn.Req[Q, error], queryArg Q,
	quitChan chan struct{}) error {

	return fn.ElimEither(
		fn.Iden, fn.Iden,
		sendQuery(sendChan, queryArg, quitChan).Either,
	)
}

// EndpointsMap is a map of all registered endpoints.
type EndpointsMap map[EndPointName]MsgEndpoint

// MultiMsgRouter is a type of message router that is capable of routing new
// incoming messages, permitting a message to be routed to multiple registered
// endpoints.
type MultiMsgRouter struct {
	startOnce sync.Once
	stopOnce  sync.Once

	// registerChan is the channel that all new endpoints will be sent to.
	registerChan chan fn.Req[MsgEndpoint, error]

	// unregisterChan is the channel that all endpoints that are to be
	// removed are sent to.
	unregisterChan chan fn.Req[EndPointName, error]

	// msgChan is the channel that all messages will be sent to for
	// processing.
	msgChan chan fn.Req[lnwire.Message, error]

	// endpointsQueries is a channel that all queries to the endpoints map
	// will be sent to.
	endpointQueries chan fn.Req[MsgEndpoint, EndpointsMap]

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewMultiMsgRouter creates a new instance of a peer message router.
func NewMultiMsgRouter() *MultiMsgRouter {
	return &MultiMsgRouter{
		registerChan:    make(chan fn.Req[MsgEndpoint, error]),
		unregisterChan:  make(chan fn.Req[EndPointName, error]),
		msgChan:         make(chan fn.Req[lnwire.Message, error]),
		endpointQueries: make(chan fn.Req[MsgEndpoint, EndpointsMap]),
		quit:            make(chan struct{}),
	}
}

// Start starts the peer message router.
func (p *MultiMsgRouter) Start() {
	peerLog.Infof("Starting MsgRouter")

	p.startOnce.Do(func() {
		p.wg.Add(1)
		go p.msgRouter()
	})
}

// Stop stops the peer message router.
func (p *MultiMsgRouter) Stop() {
	peerLog.Infof("Stopping MsgRouter")

	p.stopOnce.Do(func() {
		close(p.quit)
		p.wg.Wait()
	})
}

// RegisterEndpoint registers a new endpoint with the router. If a duplicate
// endpoint exists, an error is returned.
func (p *MultiMsgRouter) RegisterEndpoint(endpoint MsgEndpoint) error {
	return sendQueryErr(p.registerChan, endpoint, p.quit)
}

// UnregisterEndpoint unregisters the target endpoint from the router.
func (p *MultiMsgRouter) UnregisterEndpoint(name EndPointName) error {
	return sendQueryErr(p.unregisterChan, name, p.quit)
}

// RouteMsg attempts to route the target message to a registered endpoint. If
// ANY endpoint could handle the message, then nil is returned.
func (p *MultiMsgRouter) RouteMsg(msg lnwire.Message) error {
	return sendQueryErr(p.msgChan, msg, p.quit)
}

// Endpoints returns a list of all registered endpoints.
func (p *MultiMsgRouter) Endpoints() EndpointsMap {
	return fn.ElimEither(
		fn.Iden, fn.Const[EndpointsMap, error](nil),
		sendQuery(p.endpointQueries, nil, p.quit).Either,
	)
}

// msgRouter is the main goroutine that handles all incoming messages.
func (p *MultiMsgRouter) msgRouter() {
	defer p.wg.Done()

	// endpoints is a map of all registered endpoints.
	endpoints := make(map[EndPointName]MsgEndpoint)

	for {
		select {
		// A new endpoint was just sent in, so we'll add it to our set
		// of registered endpoints.
		case newEndpointMsg := <-p.registerChan:
			endpoint := newEndpointMsg.Request

			peerLog.Infof("MsgRouter: registering new "+
				"MsgEndpoint(%s)", endpoint.Name())

			// If this endpoint already exists, then we'll return
			// an error as we require unique names.
			if _, ok := endpoints[endpoint.Name()]; ok {
				peerLog.Errorf("MsgRouter: rejecting "+
					"duplicate endpoint: %v",
					endpoint.Name())

				newEndpointMsg.Resolve(ErrDuplicateEndpoint)

				continue
			}

			endpoints[endpoint.Name()] = endpoint

			newEndpointMsg.Resolve(nil)

		// A request to unregister an endpoint was just sent in, so
		// we'll attempt to remove it.
		case endpointName := <-p.unregisterChan:
			delete(endpoints, endpointName.Request)

			peerLog.Infof("MsgRouter: unregistering "+
				"MsgEndpoint(%s)", endpointName.Request)

			endpointName.Resolve(nil)

		// A new message was just sent in. We'll attempt to route it to
		// all the endpoints that can handle it.
		case msgQuery := <-p.msgChan:
			msg := msgQuery.Request

			// Loop through all the endpoints and send the message
			// to those that can handle it the message.
			var couldSend bool
			for _, endpoint := range endpoints {
				if endpoint.CanHandle(msg) {
					peerLog.Tracef("MsgRouter: sending "+
						"msg %T to endpoint %s", msg,
						endpoint.Name())

					sent := endpoint.SendMessage(msg)
					couldSend = couldSend || sent
				}
			}

			var err error
			if !couldSend {
				peerLog.Tracef("MsgRouter: unable to route "+
					"msg %T", msg)

				err = ErrUnableToRouteMsg
			}

			msgQuery.Resolve(err)

		// A query for the endpoint state just came in, we'll send back
		// a copy of our current state.
		case endpointQuery := <-p.endpointQueries:
			endpointsCopy := make(EndpointsMap, len(endpoints))
			maps.Copy(endpointsCopy, endpoints)

			endpointQuery.Resolve(endpointsCopy)

		case <-p.quit:
			return
		}
	}
}

// A compile time check to ensure MultiMsgRouter implements the MsgRouter
// interface.
var _ MsgRouter = (*MultiMsgRouter)(nil)
