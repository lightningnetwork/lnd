package subscribe

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/queue"
)

// ErrServerShuttingDown is an error returned in case the server is in the
// process of shutting down.
var ErrServerShuttingDown = errors.New("subscription server shutting down")

// Client is used to get notified about updates the caller has subscribed to,
type Client struct {
	// cancel should be called in case the client no longer wants to
	// subscribe for updates from the server.
	cancel func()

	updates *queue.ConcurrentQueue
	quit    chan struct{}
}

// Updates returns a read-only channel where the updates the client has
// subscribed to will be delivered.
func (c *Client) Updates() <-chan interface{} {
	return c.updates.ChanOut()
}

// Quit is a channel that will be closed in case the server decides to no
// longer deliver updates to this client.
func (c *Client) Quit() <-chan struct{} {
	return c.quit
}

// Cancel should be called in case the client no longer wants to
// subscribe for updates from the server.
func (c *Client) Cancel() {
	c.cancel()
}

// Server is a struct that manages a set of subscriptions and their
// corresponding clients. Any update will be delivered to all active clients.
type Server struct {
	clientCounter uint64 // To be used atomically.

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	clients       map[uint64]*Client
	clientUpdates chan *clientUpdate

	updates chan interface{}

	quit chan struct{}
	wg   sync.WaitGroup
}

// clientUpdate is an internal message sent to the subscriptionHandler to
// either register a new client for subscription or cancel an existing
// subscription.
type clientUpdate struct {
	// cancel indicates if the update to the client is cancelling an
	// existing client's subscription. If not then this update will be to
	// subscribe a new client.
	cancel bool

	// clientID is the unique identifier for this client. Any further
	// updates (deleting or adding) to this notification client will be
	// dispatched according to the target clientID.
	clientID uint64

	// client is the new client that will receive updates. Will be nil in
	// case this is a cancellation update.
	client *Client
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{
		clients:       make(map[uint64]*Client),
		clientUpdates: make(chan *clientUpdate),
		updates:       make(chan interface{}),
		quit:          make(chan struct{}),
	}
}

// Start starts the Server, making it ready to accept subscriptions and
// updates.
func (s *Server) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	s.wg.Add(1)
	go s.subscriptionHandler()

	return nil
}

// Stop stops the server.
func (s *Server) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	close(s.quit)
	s.wg.Wait()

	return nil
}

// Subscribe returns a Client that will receive updates any time the Server is
// made aware of a new event.
func (s *Server) Subscribe() (*Client, error) {
	// We'll first atomically obtain the next ID for this client from the
	// incrementing client ID counter.
	clientID := atomic.AddUint64(&s.clientCounter, 1)

	// Create the client that will be returned. The Cancel method is
	// populated to send the cancellation intent to the
	// subscriptionHandler.
	client := &Client{
		updates: queue.NewConcurrentQueue(20),
		quit:    make(chan struct{}),
		cancel: func() {
			select {
			case s.clientUpdates <- &clientUpdate{
				cancel:   true,
				clientID: clientID,
			}:
			case <-s.quit:
				return
			}
		},
	}

	select {
	case s.clientUpdates <- &clientUpdate{
		cancel:   false,
		clientID: clientID,
		client:   client,
	}:
	case <-s.quit:
		return nil, ErrServerShuttingDown
	}

	return client, nil
}

// SendUpdate is called to send the passed update to all currently active
// subscription clients.
func (s *Server) SendUpdate(update interface{}) error {

	select {
	case s.updates <- update:
		return nil
	case <-s.quit:
		return ErrServerShuttingDown
	}
}

// subscriptionHandler is the main handler for the Server. It will handle
// incoming updates and subscriptions, and forward the incoming updates to the
// registered clients.
//
// NOTE: MUST be run as a goroutine.
func (s *Server) subscriptionHandler() {
	defer s.wg.Done()

	for {
		select {

		// If a client update is received, the either a new
		// subscription becomes active, or we cancel and existing one.
		case update := <-s.clientUpdates:
			clientID := update.clientID

			// In case this is a cancellation, stop the client's
			// underlying queue, and remove the client from the set
			// of active subscription clients.
			if update.cancel {
				client, ok := s.clients[update.clientID]
				if ok {
					client.updates.Stop()
					close(client.quit)
					delete(s.clients, clientID)
				}

				continue
			}

			// If this was not a cancellation, start the underlying
			// queue and add the client to our set of subscription
			// clients. It will be notified about any new updates
			// the server receives.
			update.client.updates.Start()
			s.clients[update.clientID] = update.client

		// A new update was received, forward it to all active clients.
		case upd := <-s.updates:
			for _, client := range s.clients {
				select {
				case client.updates.ChanIn() <- upd:
				case <-client.quit:
				case <-s.quit:
					return
				}
			}

		// In case the server is shutting down, stop the clients and
		// close the quit channels to notify them.
		case <-s.quit:
			for _, client := range s.clients {
				client.updates.Stop()
				close(client.quit)
			}
			return
		}
	}
}
