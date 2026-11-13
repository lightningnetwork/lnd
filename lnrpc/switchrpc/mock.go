//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// mockPayer is a mock implementation of the htlcswitch.Payer interface.
type mockPayer struct {
	sendErr         error
	sentFirstHop    *lnwire.ShortChannelID
	getResultResult *htlcswitch.PaymentResult
	getResultErr    error
	resultChan      chan *htlcswitch.PaymentResult
}

// SendHTLC is a mock implementation of the SendHTLC method.
func (m *mockPayer) SendHTLC(firstHop lnwire.ShortChannelID,
	attemptID uint64, htlc *lnwire.UpdateAddHTLC) error {

	m.sentFirstHop = &firstHop

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

// mockChannelInfoAccessor is a mock implementation of the ChannelInfoAccessor
// interface that returns a fixed set of links.
type mockChannelInfoAccessor struct {
	links []htlcswitch.ChannelInfoProvider
	err   error
}

// GetLinksByPubkey returns the mocked links and error.
func (m *mockChannelInfoAccessor) GetLinksByPubkey(
	_ [33]byte) ([]htlcswitch.ChannelInfoProvider, error) {

	return m.links, m.err
}

// mockChannelLink is a mock channel link exposing only what the SendOnion
// handler reads when choosing a first hop.
type mockChannelLink struct {
	htlcswitch.ChannelInfoProvider
	scid      lnwire.ShortChannelID
	eligible  bool
	bandwidth lnwire.MilliSatoshi
}

// ShortChanID returns the mocked short channel ID.
func (m *mockChannelLink) ShortChanID() lnwire.ShortChannelID {
	return m.scid
}

// EligibleToForward returns whether the mocked link may forward.
func (m *mockChannelLink) EligibleToForward() bool {
	return m.eligible
}

// Bandwidth returns the mocked bandwidth.
func (m *mockChannelLink) Bandwidth() lnwire.MilliSatoshi {
	return m.bandwidth
}

// MayAddOutgoingHtlc always permits the HTLC.
func (m *mockChannelLink) MayAddOutgoingHtlc(lnwire.MilliSatoshi) error {
	return nil
}

// mockAttemptStore is a mock implementation of the AttemptStore interface.
type mockAttemptStore struct {
	htlcswitch.AttemptStore
	initErr    error
	failErr    error
	failCalls  int
	deleteErr  error
	deleteResp map[uint64]htlcswitch.DeletionStatus
}

// InitAttempt returns the mocked initErr.
func (m *mockAttemptStore) InitAttempt(attemptID uint64) error {
	return m.initErr
}

// FailPendingAttempt records the call and returns the mocked failErr.
func (m *mockAttemptStore) FailPendingAttempt(attemptID uint64,
	reason *htlcswitch.LinkError) error {

	m.failCalls++

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
