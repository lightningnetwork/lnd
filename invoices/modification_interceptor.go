package invoices

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/fn/v2"
)

var (
	// ErrInterceptorClientAlreadyConnected is an error that is returned
	// when a client tries to connect to the interceptor service while
	// another client is already connected.
	ErrInterceptorClientAlreadyConnected = errors.New(
		"interceptor client already connected",
	)

	// ErrInterceptorClientDisconnected is an error that is returned when
	// the client disconnects during an interceptor session.
	ErrInterceptorClientDisconnected = errors.New(
		"interceptor client disconnected",
	)
)

// safeCallback is a wrapper around a callback function that is safe for
// concurrent access.
type safeCallback struct {
	// callback is the actual callback function that is called when an
	// invoice is intercepted. This might be nil if no client is currently
	// connected.
	callback atomic.Pointer[HtlcModifyCallback]
}

// Set atomically sets the callback function. If a callback is already set, an
// error is returned. The returned function can be used to reset the callback to
// nil once the client is done.
func (s *safeCallback) Set(callback HtlcModifyCallback) (func(), error) {
	if !s.callback.CompareAndSwap(nil, &callback) {
		return nil, ErrInterceptorClientAlreadyConnected
	}

	return func() {
		s.callback.Store(nil)
	}, nil
}

// IsConnected returns true if a client is currently connected.
func (s *safeCallback) IsConnected() bool {
	return s.callback.Load() != nil
}

// Exec executes the callback function if it is set. If the callback is not set,
// an error is returned.
func (s *safeCallback) Exec(req HtlcModifyRequest) (*HtlcModifyResponse,
	error) {

	callback := s.callback.Load()
	if callback == nil {
		return nil, ErrInterceptorClientDisconnected
	}

	return (*callback)(req)
}

// HtlcModificationInterceptor is a service that intercepts HTLCs that aim to
// settle an invoice, enabling a subscribed client to modify certain aspects of
// those HTLCs.
type HtlcModificationInterceptor struct {
	started atomic.Bool
	stopped atomic.Bool

	// callback is the wrapped client callback function that is called when
	// an invoice is intercepted. This function gives the client the ability
	// to determine how the invoice should be settled.
	callback *safeCallback

	// quit is a channel that is closed when the interceptor is stopped.
	quit chan struct{}
}

// NewHtlcModificationInterceptor creates a new HtlcModificationInterceptor.
func NewHtlcModificationInterceptor() *HtlcModificationInterceptor {
	return &HtlcModificationInterceptor{
		callback: &safeCallback{},
		quit:     make(chan struct{}),
	}
}

// Intercept generates a new intercept session for the given invoice. The call
// blocks until the client has responded to the request or an error occurs. The
// response callback is only called if a session was created in the first place,
// which is only the case if a client is registered.
func (s *HtlcModificationInterceptor) Intercept(clientRequest HtlcModifyRequest,
	responseCallback func(HtlcModifyResponse)) error {

	// If there is no client callback set we will not handle the invoice
	// further.
	if !s.callback.IsConnected() {
		log.Debugf("Not intercepting invoice with circuit key %v, no "+
			"intercept client connected",
			clientRequest.ExitHtlcCircuitKey)

		return nil
	}

	// We'll block until the client has responded to the request or an error
	// occurs.
	var (
		responseChan = make(chan *HtlcModifyResponse, 1)
		errChan      = make(chan error, 1)
	)

	// The callback function will block at the client's discretion. We will
	// therefore execute it in a separate goroutine. We don't need a wait
	// group because we wait for the response directly below. The caller
	// needs to make sure they don't block indefinitely, by selecting on the
	// quit channel they receive when registering the callback.
	go func() {
		log.Debugf("Waiting for client response from invoice HTLC "+
			"interceptor session with circuit key %v",
			clientRequest.ExitHtlcCircuitKey)

		// By this point, we've already checked that the client callback
		// is set. However, if the client disconnected since that check
		// then Exec will return an error.
		result, err := s.callback.Exec(clientRequest)
		if err != nil {
			_ = fn.SendOrQuit(errChan, err, s.quit)

			return
		}

		_ = fn.SendOrQuit(responseChan, result, s.quit)
	}()

	// Wait for the client to respond or an error to occur.
	select {
	case response := <-responseChan:
		responseCallback(*response)

		return nil

	case err := <-errChan:
		log.Errorf("Error from invoice HTLC interceptor session: %v",
			err)

		return err

	case <-s.quit:
		return ErrInterceptorClientDisconnected
	}
}

// RegisterInterceptor sets the client callback function that will be called
// when an invoice is intercepted. If a callback is already set, an error is
// returned. The returned function must be used to reset the callback to nil
// once the client is done or disconnects.
func (s *HtlcModificationInterceptor) RegisterInterceptor(
	callback HtlcModifyCallback) (func(), <-chan struct{}, error) {

	done, err := s.callback.Set(callback)
	return done, s.quit, err
}

// Start starts the service.
func (s *HtlcModificationInterceptor) Start() error {
	log.Info("HtlcModificationInterceptor starting...")

	if !s.started.CompareAndSwap(false, true) {
		return fmt.Errorf("HtlcModificationInterceptor started more" +
			"than once")
	}

	log.Debugf("HtlcModificationInterceptor started")

	return nil
}

// Stop stops the service.
func (s *HtlcModificationInterceptor) Stop() error {
	log.Info("HtlcModificationInterceptor stopping...")

	if !s.stopped.CompareAndSwap(false, true) {
		return fmt.Errorf("HtlcModificationInterceptor stopped more" +
			"than once")
	}

	close(s.quit)

	log.Debug("HtlcModificationInterceptor stopped")

	return nil
}

// Ensure that HtlcModificationInterceptor implements the HtlcInterceptor and
// HtlcModifier interfaces.
var _ HtlcInterceptor = (*HtlcModificationInterceptor)(nil)
var _ HtlcModifier = (*HtlcModificationInterceptor)(nil)
