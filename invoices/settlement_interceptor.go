package invoices

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

// InterceptSession creates a session that is returned to the caller when an
// invoice is submitted to this service. This session allows the caller to block
// until the invoice is processed.
type InterceptSession struct {
	HtlcModifyRequest

	// ClientResponseChannel is a channel that is populated with the
	// client's interceptor response during an interceptor session.
	ClientResponseChannel chan HtlcModifyResponse

	// ClientErrChannel is a channel that is populated with any errors that
	// occur during the client's interceptor session.
	ClientErrChannel chan error

	// Quit is a channel that is closed when the session is no longer
	// needed.
	Quit chan struct{}
}

// HtlcModificationInterceptor is a service that intercepts HTLCs that aim to
// settle an invoice, enabling a subscribed client to modify certain aspects of
// those HTLCs.
type HtlcModificationInterceptor struct {
	wg sync.WaitGroup

	// mu is a mutex that protects the callback and activeSessions fields.
	mu sync.Mutex

	// callback is the client callback function that is called when an
	// invoice is intercepted. This function gives the client the ability to
	// determine how the invoice should be settled.
	callback HtlcModifyCallback

	// activeSessions is a map of active intercept sessions that are used to
	// manage the client query/response for a given invoice payment hash.
	activeSessions map[CircuitKey]InterceptSession
}

// NewHtlcModificationInterceptor creates a new HtlcModificationInterceptor.
func NewHtlcModificationInterceptor() *HtlcModificationInterceptor {
	return &HtlcModificationInterceptor{
		activeSessions: make(map[CircuitKey]InterceptSession),
	}
}

// Intercept generates a new intercept session for the given invoice HTLC. The
// session is returned to the caller so that they can block until the client
// resolution is received.
func (s *HtlcModificationInterceptor) Intercept(
	clientRequest HtlcModifyRequest) fn.Option[InterceptSession] {

	s.mu.Lock()
	defer s.mu.Unlock()

	// If there is no client callback set we will not handle the invoice
	// further.
	if s.callback == nil {
		return fn.None[InterceptSession]()
	}

	// Create and store a new intercept session for the invoice. We will use
	// the payment hash as the storage/retrieval key for the session.
	sessionKey := clientRequest.ExitHtlcCircuitKey
	session := InterceptSession{
		HtlcModifyRequest:     clientRequest,
		ClientResponseChannel: make(chan HtlcModifyResponse, 1),
		ClientErrChannel:      make(chan error, 1),
		Quit:                  make(chan struct{}, 1),
	}
	s.activeSessions[sessionKey] = session

	// The callback function will block at the client's discretion. We will
	// therefore execute it in a separate goroutine.
	s.wg.Add(1)
	go func(cb HtlcModifyCallback) {
		defer s.wg.Done()

		// By this point, we've already checked that the client callback
		// is set. However, if the client callback has been set to nil
		// since that check then Exec will return an error.
		err := cb(clientRequest)
		if err != nil {
			err = fmt.Errorf("client callback failed: %w", err)
			log.Error(err)
			session.ClientErrChannel <- err
		}
	}(s.callback)

	// Return the session to the caller so that they can block until the
	// resolution is received.
	return fn.Some(session)
}

// Modify changes parts of the HTLC based on the client's response.
func (s *HtlcModificationInterceptor) Modify(htlc CircuitKey,
	amountPaid lnwire.MilliSatoshi) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve the intercept session for the invoice payment hash.
	session, ok := s.activeSessions[htlc]
	if !ok {
		return fmt.Errorf("invoice intercept session not found "+
			"(circuit_key=%s)", htlc)
	}

	// Send the resolution to the session resolution channel.
	resolution := HtlcModifyResponse{
		AmountPaid: amountPaid,
	}
	sendSuccessful := fn.SendOrQuit(
		session.ClientResponseChannel, resolution, session.Quit,
	)
	if !sendSuccessful {
		return fmt.Errorf("failed to send modification to client")
	}

	return nil
}

// SetClientCallback sets the client callback function that will be called when
// an invoice is intercepted.
func (s *HtlcModificationInterceptor) SetClientCallback(
	callback HtlcModifyCallback) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.callback = callback
}

// QuitSession closes the quit channel for the session associated with the
// given invoice. This signals to the client that the session has ended.
func (s *HtlcModificationInterceptor) QuitSession(
	session InterceptSession) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve the intercept session and delete it from the local cache.
	sessionKey := session.ExitHtlcCircuitKey
	activeSession, ok := s.activeSessions[sessionKey]
	if !ok {
		// If the session is not found, no further action is necessary.
		return nil
	}

	// Send to the quit channel to signal the client that the session has
	// ended.
	close(activeSession.Quit)
	delete(s.activeSessions, sessionKey)

	return nil
}

// QuitActiveSessions quits all active sessions by sending on each session quit
// channel.
func (s *HtlcModificationInterceptor) QuitActiveSessions() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.activeSessions {
		session := s.activeSessions[key]
		close(session.Quit)
		delete(s.activeSessions, key)
	}

	return nil
}

// Start starts the service.
func (s *HtlcModificationInterceptor) Start() error {
	return nil
}

// Stop stops the service.
func (s *HtlcModificationInterceptor) Stop() error {
	// If the service is stopping, we will quit all active sessions.
	return s.QuitActiveSessions()
}

// Ensure that HtlcModificationInterceptor implements the HtlcAcceptor and
// HtlcModifier interfaces.
var _ HtlcModifier = (*HtlcModificationInterceptor)(nil)

// MockHtlcModifier is a mock implementation of the HtlcModifier interface.
type MockHtlcModifier struct {
}

// Intercept generates a new intercept session for the given invoice. The
// session is returned to the caller so that they can block until the client
// resolution is received.
func (m *MockHtlcModifier) Intercept(
	_ HtlcModifyRequest) fn.Option[InterceptSession] {

	return fn.None[InterceptSession]()
}

// SetClientCallback sets the client callback function that will be called when
// an invoice is intercepted.
func (m *MockHtlcModifier) SetClientCallback(HtlcModifyCallback) {
}

// Modify changes parts of the HTLC based on the client's response.
func (m *MockHtlcModifier) Modify(CircuitKey,
	lnwire.MilliSatoshi) error {

	return nil
}

// Ensure that MockHtlcModifier implements the HtlcAcceptor
// interface.
var _ HtlcModifier = (*MockHtlcModifier)(nil)
