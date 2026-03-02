//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	sync "sync"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// mockPayer is a mock implementation of the htlcswitch.Payer interface.
type mockPayer struct {
	sendErr         error
	getResultResult *htlcswitch.PaymentResult
	getResultErr    error
	resultChan      chan *htlcswitch.PaymentResult
}

// SendHTLC is a mock implementation of the SendHTLC method.
func (m *mockPayer) SendHTLC(firstHop lnwire.ShortChannelID,
	attemptID uint64, htlc *lnwire.UpdateAddHTLC) error {

	return m.sendErr
}

// GetAttemptResult is a mock implementation of the GetAttemptResult method.
func (m *mockPayer) GetAttemptResult(attemptID uint64,
	paymentHash lntypes.Hash,
	errorDecryptor htlcswitch.ErrorDecrypter) (
	<-chan *htlcswitch.PaymentResult, error) {

	if m.getResultErr != nil {
		return nil, m.getResultErr
	}

	// If a result channel is provided, use it.
	if m.resultChan != nil {
		return m.resultChan, nil
	}

	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	if m.getResultResult != nil {
		resultChan <- m.getResultResult
	}

	return resultChan, nil
}

// GetAttemptResult is a mock implementation of the GetAttemptResult method.
func (m *mockPayer) HasAttemptResult(attemptID uint64) (bool, error) {
	return true, nil
}

// CleanStore is a mock implementation of the CleanStore method.
func (m *mockPayer) CleanStore(keepPids map[uint64]struct{}) error {
	return nil
}

// mockRouteProcessor is a mock implementation of the routing.RouteProcessor
// interface.
type mockRouteProcessor struct {
	unmarshallErr   error
	unmarshallRoute *route.Route
}

// UnmarshallRoute is a mock implementation of the UnmarshallRoute method.
func (m *mockRouteProcessor) UnmarshallRoute(route *lnrpc.Route) (
	*route.Route, error) {

	return m.unmarshallRoute, m.unmarshallErr
}

// mockAttemptStore is a mock implementation of the AttemptStore interface.
type mockAttemptStore struct {
	htlcswitch.AttemptStore
	initErr    error
	failErr    error
	deleteErr  error
	deleteResp map[uint64]htlcswitch.DeletionStatus
}

// InitAttempt returns the mocked initErr.
func (m *mockAttemptStore) InitAttempt(attemptID uint64) error {
	return m.initErr
}

// FailPendingAttempt returns the mocked failErr.
func (m *mockAttemptStore) FailPendingAttempt(attemptID uint64,
	reason *htlcswitch.LinkError) error {

	return m.failErr
}

// DeleteAttempts returns the mocked deleteResp and deleteErr.
func (m *mockAttemptStore) DeleteAttempts(
	attemptIDs []uint64) (map[uint64]htlcswitch.DeletionStatus, error) {

	if m.deleteErr != nil {
		return nil, m.deleteErr
	}

	return m.deleteResp, nil
}

// statefulAttemptStore is a mock that tracks initialized attempt IDs to
// simulate real idempotency behavior. Unlike the basic mockAttemptStore, this
// mock maintains state across calls, making it suitable for testing multi-step
// interactions between SendOnion and DeleteAttempts.
type statefulAttemptStore struct {
	mockAttemptStore

	mu          sync.Mutex
	initialized map[uint64]bool
}

// InitAttempt records the attempt ID and returns ErrPaymentIDAlreadyExists if
// the ID was already initialized.
func (s *statefulAttemptStore) InitAttempt(attemptID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized[attemptID] {
		return htlcswitch.ErrPaymentIDAlreadyExists
	}

	s.initialized[attemptID] = true

	return nil
}

// DeleteAttempts removes initialized IDs and reports per-ID results.
func (s *statefulAttemptStore) DeleteAttempts(
	attemptIDs []uint64) (map[uint64]htlcswitch.DeletionStatus, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	results := make(map[uint64]htlcswitch.DeletionStatus)
	for _, id := range attemptIDs {
		if s.initialized[id] {
			delete(s.initialized, id)
			results[id] = htlcswitch.DeletionOK
		} else {
			results[id] = htlcswitch.DeletionNotFound
		}
	}

	return results, nil
}

// mockErrorDecrypter is a mock implementation of htlcswitch.ErrorDecrypter.
type mockErrorDecrypter struct {
	decryptedErr htlcswitch.ForwardingError
	receivedData lnwire.OpaqueReason
}

func (m *mockErrorDecrypter) DecryptError(
	err lnwire.OpaqueReason) (*htlcswitch.ForwardingError, error) {

	m.receivedData = err

	return &m.decryptedErr, nil
}
