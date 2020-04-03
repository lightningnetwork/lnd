package routing

import (
	"fmt"
	"sync"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

type mockPaymentAttemptDispatcher struct {
	onPayment func(firstHop lnwire.ShortChannelID) ([32]byte, error)
	results   map[uint64]*htlcswitch.PaymentResult

	sync.Mutex
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcher)(nil)

func (m *mockPaymentAttemptDispatcher) SendHTLC(firstHop lnwire.ShortChannelID,
	pid uint64,
	_ *lnwire.UpdateAddHTLC) error {

	if m.onPayment == nil {
		return nil
	}

	var result *htlcswitch.PaymentResult
	preimage, err := m.onPayment(firstHop)
	if err != nil {
		rtErr, ok := err.(htlcswitch.ClearTextError)
		if !ok {
			return err
		}
		result = &htlcswitch.PaymentResult{
			Error: rtErr,
		}
	} else {
		result = &htlcswitch.PaymentResult{Preimage: preimage}
	}

	m.Lock()
	if m.results == nil {
		m.results = make(map[uint64]*htlcswitch.PaymentResult)
	}

	m.results[pid] = result
	m.Unlock()

	return nil
}

func (m *mockPaymentAttemptDispatcher) GetPaymentResult(paymentID uint64,
	_ lntypes.Hash, _ htlcswitch.ErrorDecrypter) (
	<-chan *htlcswitch.PaymentResult, error) {

	c := make(chan *htlcswitch.PaymentResult, 1)

	m.Lock()
	res, ok := m.results[paymentID]
	m.Unlock()

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

func (m *mockPaymentSessionSource) NewPaymentSession(
	_ *LightningPayment) (PaymentSession, error) {

	return &mockPaymentSession{m.routes}, nil
}

func (m *mockPaymentSessionSource) NewPaymentSessionForRoute(
	preBuiltRoute *route.Route) PaymentSession {
	return nil
}

func (m *mockPaymentSessionSource) NewPaymentSessionEmpty() PaymentSession {
	return &mockPaymentSession{}
}

type mockMissionControl struct {
}

var _ MissionController = (*mockMissionControl)(nil)

func (m *mockMissionControl) ReportPaymentFail(paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*channeldb.FailureReason, error) {

	// Report a permanent failure if this is an error caused
	// by incorrect details.
	if failure.Code() == lnwire.CodeIncorrectOrUnknownPaymentDetails {
		reason := channeldb.FailureReasonPaymentDetails
		return &reason, nil
	}

	return nil, nil
}

func (m *mockMissionControl) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	return nil
}

func (m *mockMissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	return 0
}

type mockPaymentSession struct {
	routes []*route.Route
}

var _ PaymentSession = (*mockPaymentSession)(nil)

func (m *mockPaymentSession) RequestRoute(_, _ lnwire.MilliSatoshi,
	_, height uint32) (*route.Route, error) {

	if len(m.routes) == 0 {
		return nil, errNoPathFound
	}

	r := m.routes[0]
	m.routes = m.routes[1:]

	return r, nil
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

func (m *mockPayer) GetPaymentResult(paymentID uint64, _ lntypes.Hash,
	_ htlcswitch.ErrorDecrypter) (<-chan *htlcswitch.PaymentResult, error) {

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

type registerAttemptArgs struct {
	a *channeldb.HTLCAttemptInfo
}

type settleAttemptArgs struct {
	preimg lntypes.Preimage
}

type failAttemptArgs struct {
	reason *channeldb.HTLCFailInfo
}

type failPaymentArgs struct {
	reason channeldb.FailureReason
}

type testPayment struct {
	info     channeldb.PaymentCreationInfo
	attempts []channeldb.HTLCAttempt
}

type mockControlTower struct {
	payments   map[lntypes.Hash]*testPayment
	successful map[lntypes.Hash]struct{}
	failed     map[lntypes.Hash]channeldb.FailureReason

	init            chan initArgs
	registerAttempt chan registerAttemptArgs
	settleAttempt   chan settleAttemptArgs
	failAttempt     chan failAttemptArgs
	failPayment     chan failPaymentArgs
	fetchInFlight   chan struct{}

	sync.Mutex
}

var _ ControlTower = (*mockControlTower)(nil)

func makeMockControlTower() *mockControlTower {
	return &mockControlTower{
		payments:   make(map[lntypes.Hash]*testPayment),
		successful: make(map[lntypes.Hash]struct{}),
		failed:     make(map[lntypes.Hash]channeldb.FailureReason),
	}
}

func (m *mockControlTower) InitPayment(phash lntypes.Hash,
	c *channeldb.PaymentCreationInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.init != nil {
		m.init <- initArgs{c}
	}

	// Don't allow re-init a successful payment.
	if _, ok := m.successful[phash]; ok {
		return channeldb.ErrAlreadyPaid
	}

	_, failed := m.failed[phash]
	_, ok := m.payments[phash]

	// If the payment is known, only allow re-init if failed.
	if ok && !failed {
		return channeldb.ErrPaymentInFlight
	}

	delete(m.failed, phash)
	m.payments[phash] = &testPayment{
		info: *c,
	}

	return nil
}

func (m *mockControlTower) RegisterAttempt(phash lntypes.Hash,
	a *channeldb.HTLCAttemptInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.registerAttempt != nil {
		m.registerAttempt <- registerAttemptArgs{a}
	}

	// Cannot register attempts for successful or failed payments.
	if _, ok := m.successful[phash]; ok {
		return channeldb.ErrPaymentAlreadySucceeded
	}

	if _, ok := m.failed[phash]; ok {
		return channeldb.ErrPaymentAlreadyFailed
	}

	p, ok := m.payments[phash]
	if !ok {
		return channeldb.ErrPaymentNotInitiated
	}

	p.attempts = append(p.attempts, channeldb.HTLCAttempt{
		HTLCAttemptInfo: *a,
	})
	m.payments[phash] = p

	return nil
}

func (m *mockControlTower) SettleAttempt(phash lntypes.Hash,
	pid uint64, settleInfo *channeldb.HTLCSettleInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.settleAttempt != nil {
		m.settleAttempt <- settleAttemptArgs{settleInfo.Preimage}
	}

	// Only allow setting attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return channeldb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the settle info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return channeldb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return channeldb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Settle = settleInfo

		// Mark the payment successful on first settled attempt.
		m.successful[phash] = struct{}{}
		return nil
	}

	return fmt.Errorf("pid not found")
}

func (m *mockControlTower) FailAttempt(phash lntypes.Hash, pid uint64,
	failInfo *channeldb.HTLCFailInfo) error {

	m.Lock()
	defer m.Unlock()

	if m.failAttempt != nil {
		m.failAttempt <- failAttemptArgs{failInfo}
	}

	// Only allow failing attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return channeldb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the failure info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return channeldb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return channeldb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Failure = failInfo
		return nil
	}

	return fmt.Errorf("pid not found")
}

func (m *mockControlTower) Fail(phash lntypes.Hash,
	reason channeldb.FailureReason) error {

	m.Lock()
	defer m.Unlock()

	if m.failPayment != nil {
		m.failPayment <- failPaymentArgs{reason}
	}

	// Payment must be known.
	if _, ok := m.payments[phash]; !ok {
		return channeldb.ErrPaymentNotInitiated
	}

	m.failed[phash] = reason

	return nil
}

func (m *mockControlTower) FetchPayment(phash lntypes.Hash) (
	*channeldb.MPPayment, error) {

	m.Lock()
	defer m.Unlock()

	p, ok := m.payments[phash]
	if !ok {
		return nil, channeldb.ErrPaymentNotInitiated
	}

	mp := &channeldb.MPPayment{
		Info: &p.info,
	}

	reason, ok := m.failed[phash]
	if ok {
		mp.FailureReason = &reason
	}

	// Return a copy of the current attempts.
	mp.HTLCs = append(mp.HTLCs, p.attempts...)

	return mp, nil
}

func (m *mockControlTower) FetchInFlightPayments() (
	[]*channeldb.InFlightPayment, error) {

	m.Lock()
	defer m.Unlock()

	if m.fetchInFlight != nil {
		m.fetchInFlight <- struct{}{}
	}

	// In flight are all payments not successful or failed.
	var fl []*channeldb.InFlightPayment
	for hash, p := range m.payments {
		if _, ok := m.successful[hash]; ok {
			continue
		}
		if _, ok := m.failed[hash]; ok {
			continue
		}

		var attempts []channeldb.HTLCAttemptInfo
		for _, a := range p.attempts {
			attempts = append(attempts, a.HTLCAttemptInfo)
		}
		ifl := channeldb.InFlightPayment{
			Info:     &p.info,
			Attempts: attempts,
		}

		fl = append(fl, &ifl)
	}

	return fl, nil
}

func (m *mockControlTower) SubscribePayment(paymentHash lntypes.Hash) (
	bool, chan PaymentResult, error) {

	return false, nil, errors.New("not implemented")
}
