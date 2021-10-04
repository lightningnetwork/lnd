package routing

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
)

type mockPaymentAttemptDispatcherOld struct {
	onPayment func(firstHop lnwire.ShortChannelID) ([32]byte, error)
	results   map[uint64]*htlcswitch.PaymentResult

	sync.Mutex
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcherOld)(nil)

func (m *mockPaymentAttemptDispatcherOld) SendHTLC(
	firstHop lnwire.ShortChannelID, pid uint64,
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

func (m *mockPaymentAttemptDispatcherOld) GetPaymentResult(paymentID uint64,
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
func (m *mockPaymentAttemptDispatcherOld) CleanStore(
	map[uint64]struct{}) error {

	return nil
}

func (m *mockPaymentAttemptDispatcherOld) setPaymentResult(
	f func(firstHop lnwire.ShortChannelID) ([32]byte, error)) {

	m.onPayment = f
}

type mockPaymentSessionSourceOld struct {
	routes       []*route.Route
	routeRelease chan struct{}
}

var _ PaymentSessionSource = (*mockPaymentSessionSourceOld)(nil)

func (m *mockPaymentSessionSourceOld) NewPaymentSession(
	_ *LightningPayment) (PaymentSession, error) {

	return &mockPaymentSessionOld{
		routes:  m.routes,
		release: m.routeRelease,
	}, nil
}

func (m *mockPaymentSessionSourceOld) NewPaymentSessionForRoute(
	preBuiltRoute *route.Route) PaymentSession {
	return nil
}

func (m *mockPaymentSessionSourceOld) NewPaymentSessionEmpty() PaymentSession {
	return &mockPaymentSessionOld{}
}

type mockMissionControlOld struct {
	MissionControl
}

var _ MissionController = (*mockMissionControlOld)(nil)

func (m *mockMissionControlOld) ReportPaymentFail(
	paymentID uint64, rt *route.Route,
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

func (m *mockMissionControlOld) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	return nil
}

func (m *mockMissionControlOld) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	return 0
}

type mockPaymentSessionOld struct {
	routes []*route.Route

	// release is a channel that optionally blocks requesting a route
	// from our mock payment channel. If this value is nil, we will just
	// release the route automatically.
	release chan struct{}
}

var _ PaymentSession = (*mockPaymentSessionOld)(nil)

func (m *mockPaymentSessionOld) RequestRoute(_, _ lnwire.MilliSatoshi,
	_, height uint32) (*route.Route, error) {

	if m.release != nil {
		m.release <- struct{}{}
	}

	if len(m.routes) == 0 {
		return nil, errNoPathFound
	}

	r := m.routes[0]
	m.routes = m.routes[1:]

	return r, nil
}

func (m *mockPaymentSessionOld) UpdateAdditionalEdge(_ *lnwire.ChannelUpdate,
	_ *btcec.PublicKey, _ *channeldb.CachedEdgePolicy) bool {

	return false
}

func (m *mockPaymentSessionOld) GetAdditionalEdgePolicy(_ *btcec.PublicKey,
	_ uint64) *channeldb.CachedEdgePolicy {

	return nil
}

type mockPayerOld struct {
	sendResult    chan error
	paymentResult chan *htlcswitch.PaymentResult
	quit          chan struct{}
}

var _ PaymentAttemptDispatcher = (*mockPayerOld)(nil)

func (m *mockPayerOld) SendHTLC(_ lnwire.ShortChannelID,
	paymentID uint64,
	_ *lnwire.UpdateAddHTLC) error {

	select {
	case res := <-m.sendResult:
		return res
	case <-m.quit:
		return fmt.Errorf("test quitting")
	}

}

func (m *mockPayerOld) GetPaymentResult(paymentID uint64, _ lntypes.Hash,
	_ htlcswitch.ErrorDecrypter) (<-chan *htlcswitch.PaymentResult, error) {

	select {
	case res, ok := <-m.paymentResult:
		resChan := make(chan *htlcswitch.PaymentResult, 1)
		if !ok {
			close(resChan)
		} else {
			resChan <- res
		}

		return resChan, nil

	case <-m.quit:
		return nil, fmt.Errorf("test quitting")
	}
}

func (m *mockPayerOld) CleanStore(pids map[uint64]struct{}) error {
	return nil
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

type mockControlTowerOld struct {
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

var _ ControlTower = (*mockControlTowerOld)(nil)

func makeMockControlTower() *mockControlTowerOld {
	return &mockControlTowerOld{
		payments:   make(map[lntypes.Hash]*testPayment),
		successful: make(map[lntypes.Hash]struct{}),
		failed:     make(map[lntypes.Hash]channeldb.FailureReason),
	}
}

func (m *mockControlTowerOld) InitPayment(phash lntypes.Hash,
	c *channeldb.PaymentCreationInfo) error {

	if m.init != nil {
		m.init <- initArgs{c}
	}

	m.Lock()
	defer m.Unlock()

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

func (m *mockControlTowerOld) RegisterAttempt(phash lntypes.Hash,
	a *channeldb.HTLCAttemptInfo) error {

	if m.registerAttempt != nil {
		m.registerAttempt <- registerAttemptArgs{a}
	}

	m.Lock()
	defer m.Unlock()

	// Lookup payment.
	p, ok := m.payments[phash]
	if !ok {
		return channeldb.ErrPaymentNotInitiated
	}

	var inFlight bool
	for _, a := range p.attempts {
		if a.Settle != nil {
			continue
		}

		if a.Failure != nil {
			continue
		}

		inFlight = true
	}

	// Cannot register attempts for successful or failed payments.
	_, settled := m.successful[phash]
	_, failed := m.failed[phash]

	if settled || failed {
		return channeldb.ErrPaymentTerminal
	}

	if settled && !inFlight {
		return channeldb.ErrPaymentAlreadySucceeded
	}

	if failed && !inFlight {
		return channeldb.ErrPaymentAlreadyFailed
	}

	// Add attempt to payment.
	p.attempts = append(p.attempts, channeldb.HTLCAttempt{
		HTLCAttemptInfo: *a,
	})
	m.payments[phash] = p

	return nil
}

func (m *mockControlTowerOld) SettleAttempt(phash lntypes.Hash,
	pid uint64, settleInfo *channeldb.HTLCSettleInfo) (
	*channeldb.HTLCAttempt, error) {

	if m.settleAttempt != nil {
		m.settleAttempt <- settleAttemptArgs{settleInfo.Preimage}
	}

	m.Lock()
	defer m.Unlock()

	// Only allow setting attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return nil, channeldb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the settle info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return nil, channeldb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return nil, channeldb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Settle = settleInfo

		// Mark the payment successful on first settled attempt.
		m.successful[phash] = struct{}{}
		return &channeldb.HTLCAttempt{
			Settle: settleInfo,
		}, nil
	}

	return nil, fmt.Errorf("pid not found")
}

func (m *mockControlTowerOld) FailAttempt(phash lntypes.Hash, pid uint64,
	failInfo *channeldb.HTLCFailInfo) (*channeldb.HTLCAttempt, error) {

	if m.failAttempt != nil {
		m.failAttempt <- failAttemptArgs{failInfo}
	}

	m.Lock()
	defer m.Unlock()

	// Only allow failing attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return nil, channeldb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the failure info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return nil, channeldb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return nil, channeldb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Failure = failInfo
		return &channeldb.HTLCAttempt{
			Failure: failInfo,
		}, nil
	}

	return nil, fmt.Errorf("pid not found")
}

func (m *mockControlTowerOld) Fail(phash lntypes.Hash,
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

func (m *mockControlTowerOld) FetchPayment(phash lntypes.Hash) (
	*channeldb.MPPayment, error) {

	m.Lock()
	defer m.Unlock()

	return m.fetchPayment(phash)
}

func (m *mockControlTowerOld) fetchPayment(phash lntypes.Hash) (
	*channeldb.MPPayment, error) {

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

func (m *mockControlTowerOld) FetchInFlightPayments() (
	[]*channeldb.MPPayment, error) {

	if m.fetchInFlight != nil {
		m.fetchInFlight <- struct{}{}
	}

	m.Lock()
	defer m.Unlock()

	// In flight are all payments not successful or failed.
	var fl []*channeldb.MPPayment
	for hash := range m.payments {
		if _, ok := m.successful[hash]; ok {
			continue
		}
		if _, ok := m.failed[hash]; ok {
			continue
		}

		mp, err := m.fetchPayment(hash)
		if err != nil {
			return nil, err
		}

		fl = append(fl, mp)
	}

	return fl, nil
}

func (m *mockControlTowerOld) SubscribePayment(paymentHash lntypes.Hash) (
	*ControlTowerSubscriber, error) {

	return nil, errors.New("not implemented")
}

type mockPaymentAttemptDispatcher struct {
	mock.Mock

	resultChan chan *htlcswitch.PaymentResult
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcher)(nil)

func (m *mockPaymentAttemptDispatcher) SendHTLC(firstHop lnwire.ShortChannelID,
	pid uint64, htlcAdd *lnwire.UpdateAddHTLC) error {

	args := m.Called(firstHop, pid, htlcAdd)
	return args.Error(0)
}

func (m *mockPaymentAttemptDispatcher) GetPaymentResult(attemptID uint64,
	paymentHash lntypes.Hash, deobfuscator htlcswitch.ErrorDecrypter) (
	<-chan *htlcswitch.PaymentResult, error) {

	m.Called(attemptID, paymentHash, deobfuscator)

	// Instead of returning the mocked returned values, we need to return
	// the chan resultChan so it can be converted into a read-only chan.
	return m.resultChan, nil
}

func (m *mockPaymentAttemptDispatcher) CleanStore(
	keepPids map[uint64]struct{}) error {

	args := m.Called(keepPids)
	return args.Error(0)
}

type mockPaymentSessionSource struct {
	mock.Mock
}

var _ PaymentSessionSource = (*mockPaymentSessionSource)(nil)

func (m *mockPaymentSessionSource) NewPaymentSession(
	payment *LightningPayment) (PaymentSession, error) {

	args := m.Called(payment)
	return args.Get(0).(PaymentSession), args.Error(1)
}

func (m *mockPaymentSessionSource) NewPaymentSessionForRoute(
	preBuiltRoute *route.Route) PaymentSession {

	args := m.Called(preBuiltRoute)
	return args.Get(0).(PaymentSession)
}

func (m *mockPaymentSessionSource) NewPaymentSessionEmpty() PaymentSession {
	args := m.Called()
	return args.Get(0).(PaymentSession)
}

type mockMissionControl struct {
	mock.Mock
}

var _ MissionController = (*mockMissionControl)(nil)

func (m *mockMissionControl) ReportPaymentFail(
	paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*channeldb.FailureReason, error) {

	args := m.Called(paymentID, rt, failureSourceIdx, failure)

	// Type assertion on nil will fail, so we check and return here.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*channeldb.FailureReason), args.Error(1)
}

func (m *mockMissionControl) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	args := m.Called(paymentID, rt)
	return args.Error(0)
}

func (m *mockMissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	args := m.Called(fromNode, toNode, amt)
	return args.Get(0).(float64)
}

type mockPaymentSession struct {
	mock.Mock
}

var _ PaymentSession = (*mockPaymentSession)(nil)

func (m *mockPaymentSession) RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
	activeShards, height uint32) (*route.Route, error) {
	args := m.Called(maxAmt, feeLimit, activeShards, height)
	return args.Get(0).(*route.Route), args.Error(1)
}

func (m *mockPaymentSession) UpdateAdditionalEdge(msg *lnwire.ChannelUpdate,
	pubKey *btcec.PublicKey, policy *channeldb.CachedEdgePolicy) bool {

	args := m.Called(msg, pubKey, policy)
	return args.Bool(0)
}

func (m *mockPaymentSession) GetAdditionalEdgePolicy(pubKey *btcec.PublicKey,
	channelID uint64) *channeldb.CachedEdgePolicy {

	args := m.Called(pubKey, channelID)
	return args.Get(0).(*channeldb.CachedEdgePolicy)
}

type mockControlTower struct {
	mock.Mock
	sync.Mutex
}

var _ ControlTower = (*mockControlTower)(nil)

func (m *mockControlTower) InitPayment(phash lntypes.Hash,
	c *channeldb.PaymentCreationInfo) error {

	args := m.Called(phash, c)
	return args.Error(0)
}

func (m *mockControlTower) RegisterAttempt(phash lntypes.Hash,
	a *channeldb.HTLCAttemptInfo) error {

	m.Lock()
	defer m.Unlock()

	args := m.Called(phash, a)
	return args.Error(0)
}

func (m *mockControlTower) SettleAttempt(phash lntypes.Hash,
	pid uint64, settleInfo *channeldb.HTLCSettleInfo) (
	*channeldb.HTLCAttempt, error) {

	m.Lock()
	defer m.Unlock()

	args := m.Called(phash, pid, settleInfo)
	return args.Get(0).(*channeldb.HTLCAttempt), args.Error(1)
}

func (m *mockControlTower) FailAttempt(phash lntypes.Hash, pid uint64,
	failInfo *channeldb.HTLCFailInfo) (*channeldb.HTLCAttempt, error) {

	m.Lock()
	defer m.Unlock()

	args := m.Called(phash, pid, failInfo)
	return args.Get(0).(*channeldb.HTLCAttempt), args.Error(1)
}

func (m *mockControlTower) Fail(phash lntypes.Hash,
	reason channeldb.FailureReason) error {

	m.Lock()
	defer m.Unlock()

	args := m.Called(phash, reason)
	return args.Error(0)
}

func (m *mockControlTower) FetchPayment(phash lntypes.Hash) (
	*channeldb.MPPayment, error) {

	m.Lock()
	defer m.Unlock()
	args := m.Called(phash)

	// Type assertion on nil will fail, so we check and return here.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	// Make a copy of the payment here to avoid data race.
	p := args.Get(0).(*channeldb.MPPayment)
	payment := &channeldb.MPPayment{
		FailureReason: p.FailureReason,
	}
	payment.HTLCs = make([]channeldb.HTLCAttempt, len(p.HTLCs))
	copy(payment.HTLCs, p.HTLCs)

	return payment, args.Error(1)
}

func (m *mockControlTower) FetchInFlightPayments() (
	[]*channeldb.MPPayment, error) {

	args := m.Called()
	return args.Get(0).([]*channeldb.MPPayment), args.Error(1)
}

func (m *mockControlTower) SubscribePayment(paymentHash lntypes.Hash) (
	*ControlTowerSubscriber, error) {

	args := m.Called(paymentHash)
	return args.Get(0).(*ControlTowerSubscriber), args.Error(1)
}
