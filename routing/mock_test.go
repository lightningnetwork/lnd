package routing

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
)

type mockPaymentAttemptDispatcher struct {
	onPayment func(firstHop lnwire.ShortChannelID) ([32]byte, error)
	results   map[uint64]*htlcswitch.PaymentResult
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcher)(nil)

func (m *mockPaymentAttemptDispatcher) SendHTLC(firstHop lnwire.ShortChannelID,
	pid uint64,
	_ *lnwire.UpdateAddHTLC,
	_ htlcswitch.ErrorDecrypter) error {

	if m.onPayment == nil {
		return nil
	}

	if m.results == nil {
		m.results = make(map[uint64]*htlcswitch.PaymentResult)
	}

	var result *htlcswitch.PaymentResult
	preimage, err := m.onPayment(firstHop)
	if err != nil {
		fwdErr, ok := err.(*htlcswitch.ForwardingError)
		if !ok {
			return err
		}
		result = &htlcswitch.PaymentResult{
			Error: fwdErr,
		}
	} else {
		result = &htlcswitch.PaymentResult{Preimage: preimage}
	}

	m.results[pid] = result

	return nil
}

func (m *mockPaymentAttemptDispatcher) GetPaymentResult(paymentID uint64) (
	<-chan *htlcswitch.PaymentResult, error) {

	c := make(chan *htlcswitch.PaymentResult, 1)
	res, ok := m.results[paymentID]
	if !ok {
		return nil, htlcswitch.ErrPaymentIDNotFound
	}
	c <- res

	return c, nil

}

func (m *mockPaymentAttemptDispatcher) setPaymentResult(
	f func(firstHop lnwire.ShortChannelID) ([32]byte, error)) {

	m.onPayment = f
}
