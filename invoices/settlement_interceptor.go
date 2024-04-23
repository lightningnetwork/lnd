package invoices

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
)

// SafeCallback is a thread safe wrapper around an InterceptorClientCallback.
type SafeCallback struct {
	// mu is a mutex that protects the callback field.
	mu sync.Mutex

	// callback is the client callback function that is called when an
	// invoice is intercepted. This function gives the client the ability to
	// determine how the invoice should be settled.
	callback InterceptorClientCallback
}

// Set sets the client callback function.
func (sc *SafeCallback) Set(callback InterceptorClientCallback) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.callback = callback
}

// Exec calls the client callback function.
func (sc *SafeCallback) Exec(req InterceptClientRequest) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.callback == nil {
		return fmt.Errorf("client callback not set")
	}

	return sc.callback(req)
}

// IsSet returns true if the client callback function is set.
func (sc *SafeCallback) IsSet() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	return sc.callback != nil
}

// InterceptClientResponse is the response that is sent from the client during
// an interceptor session. The response contains modifications to the invoice
// settlement process.
type InterceptClientResponse struct {
	// SkipAmountCheck is a flag that indicates whether the amount check
	// should be skipped during the invoice settlement process.
	SkipAmountCheck bool
}

// InterceptSession creates a session that is returned to the caller when an
// invoice is submitted to this service. This session allows the caller to block
// until the invoice is processed.
type InterceptSession struct {
	InterceptClientRequest

	// ClientResponseChannel is a channel that is populated with the
	// client's interceptor response during an interceptor session.
	ClientResponseChannel chan InterceptClientResponse

	// Quit is a channel that is closed when the session is no longer
	// needed.
	Quit chan struct{}
}

// SettlementInterceptor is a service that intercepts invoices during the
// settlement phase, enabling a subscribed client to determine the settlement
// outcome.
type SettlementInterceptor struct {
	wg sync.WaitGroup

	// callback is a client defined function that is called when an invoice
	// is intercepted. This function gives the client the ability to
	// determine the settlement outcome.
	clientCallback SafeCallback

	// activeSessions is a map of active intercept sessions that are used to
	// manage the client query/response for a given invoice payment hash.
	activeSessions lnutils.SyncMap[lntypes.Hash, InterceptSession]
}

// NewSettlementInterceptor creates a new SettlementInterceptor.
func NewSettlementInterceptor() *SettlementInterceptor {
	return &SettlementInterceptor{
		activeSessions: lnutils.SyncMap[
			lntypes.Hash, InterceptSession,
		]{},
	}
}

// Intercept generates a new intercept session for the given invoice. The
// session is returned to the caller so that they can block until the
// client resolution is received.
func (s *SettlementInterceptor) Intercept(
	clientRequest InterceptClientRequest) fn.Option[InterceptSession] {

	// If there is no client callback set we will not handle the invoice
	// further.
	if !s.clientCallback.IsSet() {
		return fn.None[InterceptSession]()
	}

	// Create and store a new intercept session for the invoice. We will use
	// the payment hash as the storage/retrieval key for the session.
	paymentHash := clientRequest.Invoice.Terms.PaymentPreimage.Hash()
	session := InterceptSession{
		InterceptClientRequest: clientRequest,
		ClientResponseChannel:  make(chan InterceptClientResponse, 1),
		Quit:                   make(chan struct{}, 1),
	}
	s.activeSessions.Store(paymentHash, session)

	// The callback function will block at the client's discretion. We will
	// therefore execute it in a separate goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// By this point, we've already checked that the client callback
		// is set. However, if the client callback has been set to nil
		// since that check then Exec will return an error.
		err := s.clientCallback.Exec(clientRequest)
		if err != nil {
			log.Errorf("client callback failed: %v", err)
		}
	}()

	// Return the session to the caller so that they can block until the
	// resolution is received.
	return fn.Some(session)
}

// Resolve passes a client specified resolution to the session resolution
// channel associated with the given invoice payment hash.
func (s *SettlementInterceptor) Resolve(invoicePaymentHash lntypes.Hash,
	skipAmountCheck bool) error {

	// Retrieve the intercept session for the invoice payment hash.
	session, ok := s.activeSessions.LoadAndDelete(
		invoicePaymentHash,
	)
	if !ok {
		return fmt.Errorf("invoice intercept session not found "+
			"(payment_hash=%s)", invoicePaymentHash.String())
	}

	// Send the resolution to the session resolution channel.
	resolution := InterceptClientResponse{
		SkipAmountCheck: skipAmountCheck,
	}
	sendSuccessful := fn.SendOrQuit(
		session.ClientResponseChannel, resolution, session.Quit,
	)
	if !sendSuccessful {
		return fmt.Errorf("failed to send resolution to client")
	}

	return nil
}

// SetClientCallback sets the client callback function that will be called when
// an invoice is intercepted.
func (s *SettlementInterceptor) SetClientCallback(
	callback InterceptorClientCallback) {

	s.clientCallback.Set(callback)
}

// QuitSession closes the quit channel for the session associated with the
// given invoice. This signals to the client that the session has ended.
func (s *SettlementInterceptor) QuitSession(session InterceptSession) error {
	// Retrieve the intercept session and delete it from the local cache.
	paymentHash := session.Invoice.Terms.PaymentPreimage.Hash()
	session, ok := s.activeSessions.LoadAndDelete(paymentHash)
	if !ok {
		// If the session is not found, no further action is necessary.
		return nil
	}

	// Send to the quit channel to signal the client that the session has
	// ended.
	session.Quit <- struct{}{}

	return nil
}

// QuitActiveSessions quits all active sessions by sending on each session quit
// channel.
func (s *SettlementInterceptor) QuitActiveSessions() error {
	s.activeSessions.Range(func(_ lntypes.Hash, session InterceptSession) bool { //nolint:lll
		session.Quit <- struct{}{}

		return true
	})

	// Empty the intercept sessions map.
	s.activeSessions = lnutils.SyncMap[lntypes.Hash, InterceptSession]{}

	return nil
}

// Start starts the service.
func (s *SettlementInterceptor) Start() error {
	return nil
}

// Stop stops the service.
func (s *SettlementInterceptor) Stop() error {
	// If the service is stopping, we will quit all active sessions.
	err := s.QuitActiveSessions()
	if err != nil {
		return err
	}

	return nil
}

// Ensure that SettlementInterceptor implements the HtlcResolutionInterceptor
// interface.
var _ SettlementInterceptorInterface = (*SettlementInterceptor)(nil)
