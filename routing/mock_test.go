package routing

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

type mockPaymentAttemptDispatcher struct {
	onPayment func(firstHop lnwire.ShortChannelID) ([32]byte, error)
	results   map[uint64]*htlcswitch.PaymentResult
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcher)(nil)

func (m *mockPaymentAttemptDispatcher) SendHTLC(firstHop lnwire.ShortChannelID,
	pid uint64,
	_ *lnwire.UpdateAddHTLC) error {

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

func (m *mockPaymentAttemptDispatcher) GetPaymentResult(paymentID uint64,
	_ htlcswitch.ErrorDecrypter) (<-chan *htlcswitch.PaymentResult, error) {

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

type mockPaymentSessionSource struct {
	routes []*route.Route
}

var _ PaymentSessionSource = (*mockPaymentSessionSource)(nil)

func (m *mockPaymentSessionSource) NewPaymentSession(routeHints [][]zpay32.HopHint,
	target route.Vertex) (PaymentSession, error) {

	return &mockPaymentSession{m.routes}, nil
}

func (m *mockPaymentSessionSource) NewPaymentSessionForRoute(
	preBuiltRoute *route.Route) PaymentSession {
	return nil
}

func (m *mockPaymentSessionSource) NewPaymentSessionEmpty() PaymentSession {
	return &mockPaymentSession{}
}

type mockPaymentSession struct {
	routes []*route.Route
}

var _ PaymentSession = (*mockPaymentSession)(nil)

func (m *mockPaymentSession) RequestRoute(payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (*route.Route, error) {

	if len(m.routes) == 0 {
		return nil, fmt.Errorf("no routes")
	}

	r := m.routes[0]
	m.routes = m.routes[1:]

	return r, nil
}

func (m *mockPaymentSession) ReportVertexFailure(v route.Vertex) {}

func (m *mockPaymentSession) ReportEdgeFailure(e *EdgeLocator) {}

func (m *mockPaymentSession) ReportEdgePolicyFailure(errSource route.Vertex, failedEdge *EdgeLocator) {
}

type mockPayer struct {
	sendResult       chan error
	paymentResultErr chan error
	paymentResult    chan *htlcswitch.PaymentResult
	quit             chan struct{}
}

var _ PaymentAttemptDispatcher = (*mockPayer)(nil)

func (m *mockPayer) SendHTLC(_ lnwire.ShortChannelID,
	paymentID uint64,
	_ *lnwire.UpdateAddHTLC) error {

	select {
	case res := <-m.sendResult:
		return res
	case <-m.quit:
		return fmt.Errorf("test quitting")
	}

}

func (m *mockPayer) GetPaymentResult(paymentID uint64, _ htlcswitch.ErrorDecrypter) (
	<-chan *htlcswitch.PaymentResult, error) {

	select {
	case res := <-m.paymentResult:
		resChan := make(chan *htlcswitch.PaymentResult, 1)
		resChan <- res
		return resChan, nil
	case err := <-m.paymentResultErr:
		return nil, err
	case <-m.quit:
		return nil, fmt.Errorf("test quitting")
	}
}

type initArgs struct {
	c *channeldb.PaymentCreationInfo
}

type registerArgs struct {
	a *channeldb.PaymentAttemptInfo
}

type successArgs struct {
	preimg lntypes.Preimage
}

type failArgs struct {
	reason channeldb.FailureReason
}

type mockControlTower struct {
	inflights  map[lntypes.Hash]channeldb.InFlightPayment
	successful map[lntypes.Hash]struct{}

	init          chan initArgs
	register      chan registerArgs
	success       chan successArgs
	fail          chan failArgs
	fetchInFlight chan struct{}

	sync.Mutex
}

var _ ControlTower = (*mockControlTower)(nil)

func makeMockControlTower() *mockControlTower {
	return &mockControlTower{
		inflights:  make(map[lntypes.Hash]channeldb.InFlightPayment),
		successful: make(map[lntypes.Hash]struct{}),
	}
}

func (m *mockControlTower) InitPayment(phash lntypes.Hash,
	c *channeldb.PaymentCreationInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.init != nil {
		m.init <- initArgs{c}
	}

	if _, ok := m.successful[phash]; ok {
		return fmt.Errorf("already successful")
	}

	_, ok := m.inflights[phash]
	if ok {
		return fmt.Errorf("in flight")
	}

	m.inflights[phash] = channeldb.InFlightPayment{
		Info: c,
	}

	return nil
}

func (m *mockControlTower) RegisterAttempt(phash lntypes.Hash,
	a *channeldb.PaymentAttemptInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.register != nil {
		m.register <- registerArgs{a}
	}

	p, ok := m.inflights[phash]
	if !ok {
		return fmt.Errorf("not in flight")
	}

	p.Attempt = a
	m.inflights[phash] = p

	return nil
}

func (m *mockControlTower) Success(phash lntypes.Hash,
	preimg lntypes.Preimage) error {

	m.Lock()
	defer m.Unlock()

	if m.success != nil {
		m.success <- successArgs{preimg}
	}

	delete(m.inflights, phash)
	m.successful[phash] = struct{}{}
	return nil
}

func (m *mockControlTower) Fail(phash lntypes.Hash,
	reason channeldb.FailureReason) error {

	m.Lock()
	defer m.Unlock()

	if m.fail != nil {
		m.fail <- failArgs{reason}
	}

	delete(m.inflights, phash)
	return nil
}

func (m *mockControlTower) FetchInFlightPayments() (
	[]*channeldb.InFlightPayment, error) {

	m.Lock()
	defer m.Unlock()

	if m.fetchInFlight != nil {
		m.fetchInFlight <- struct{}{}
	}

	var fl []*channeldb.InFlightPayment
	for _, ifl := range m.inflights {
		fl = append(fl, &ifl)
	}

	return fl, nil
}
