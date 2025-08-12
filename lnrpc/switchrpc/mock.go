//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
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
