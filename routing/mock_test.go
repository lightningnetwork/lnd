package routing

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/shards"
	"github.com/lightningnetwork/lnd/tlv"
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

func (m *mockPaymentAttemptDispatcherOld) HasAttemptResult(
	attemptID uint64) (bool, error) {

	return false, nil
}

func (m *mockPaymentAttemptDispatcherOld) GetAttemptResult(paymentID uint64,
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
	_ *LightningPayment, _ fn.Option[tlv.Blob],
	_ fn.Option[htlcswitch.AuxTrafficShaper]) (PaymentSession, error) {

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
	MissionController
}

var _ MissionControlQuerier = (*mockMissionControlOld)(nil)

func (m *mockMissionControlOld) ReportPaymentFail(
	paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*paymentsdb.FailureReason, error) {

	// Report a permanent failure if this is an error caused
	// by incorrect details.
	if failure.Code() == lnwire.CodeIncorrectOrUnknownPaymentDetails {
		reason := paymentsdb.FailureReasonPaymentDetails
		return &reason, nil
	}

	return nil, nil
}

func (m *mockMissionControlOld) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	return nil
}

func (m *mockMissionControlOld) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64 {

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
	_, height uint32, _ lnwire.CustomRecords) (*route.Route,
	error) {

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

func (m *mockPaymentSessionOld) UpdateAdditionalEdge(_ *lnwire.ChannelUpdate1,
	_ *btcec.PublicKey, _ *models.CachedEdgePolicy) bool {

	return false
}

func (m *mockPaymentSessionOld) GetAdditionalEdgePolicy(_ *btcec.PublicKey,
	_ uint64) *models.CachedEdgePolicy {

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

func (m *mockPayerOld) HasAttemptResult(attemptID uint64) (bool, error) {
	return false, nil
}

func (m *mockPayerOld) GetAttemptResult(paymentID uint64, _ lntypes.Hash,
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
	c *paymentsdb.PaymentCreationInfo
}

type registerAttemptArgs struct {
	a *paymentsdb.HTLCAttemptInfo
}

type settleAttemptArgs struct {
	preimg lntypes.Preimage
}

type failAttemptArgs struct {
	reason *paymentsdb.HTLCFailInfo
}

type failPaymentArgs struct {
	reason paymentsdb.FailureReason
}

type testPayment struct {
	info     paymentsdb.PaymentCreationInfo
	attempts []paymentsdb.HTLCAttempt
}

type mockControlTowerOld struct {
	payments   map[lntypes.Hash]*testPayment
	successful map[lntypes.Hash]struct{}
	failed     map[lntypes.Hash]paymentsdb.FailureReason

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
		failed:     make(map[lntypes.Hash]paymentsdb.FailureReason),
	}
}

func (m *mockControlTowerOld) InitPayment(phash lntypes.Hash,
	c *paymentsdb.PaymentCreationInfo) error {

	if m.init != nil {
		m.init <- initArgs{c}
	}

	m.Lock()
	defer m.Unlock()

	// Don't allow re-init a successful payment.
	if _, ok := m.successful[phash]; ok {
		return paymentsdb.ErrAlreadyPaid
	}

	_, failed := m.failed[phash]
	_, ok := m.payments[phash]

	// If the payment is known, only allow re-init if failed.
	if ok && !failed {
		return paymentsdb.ErrPaymentInFlight
	}

	delete(m.failed, phash)
	m.payments[phash] = &testPayment{
		info: *c,
	}

	return nil
}

func (m *mockControlTowerOld) DeleteFailedAttempts(phash lntypes.Hash) error {
	p, ok := m.payments[phash]
	if !ok {
		return paymentsdb.ErrPaymentNotInitiated
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

	if inFlight {
		return paymentsdb.ErrPaymentInFlight
	}

	return nil
}

func (m *mockControlTowerOld) RegisterAttempt(phash lntypes.Hash,
	a *paymentsdb.HTLCAttemptInfo) error {

	if m.registerAttempt != nil {
		m.registerAttempt <- registerAttemptArgs{a}
	}

	m.Lock()
	defer m.Unlock()

	// Lookup payment.
	p, ok := m.payments[phash]
	if !ok {
		return paymentsdb.ErrPaymentNotInitiated
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
		return paymentsdb.ErrPaymentTerminal
	}

	if settled && !inFlight {
		return paymentsdb.ErrPaymentAlreadySucceeded
	}

	if failed && !inFlight {
		return paymentsdb.ErrPaymentAlreadyFailed
	}

	// Add attempt to payment.
	p.attempts = append(p.attempts, paymentsdb.HTLCAttempt{
		HTLCAttemptInfo: *a,
	})
	m.payments[phash] = p

	return nil
}

func (m *mockControlTowerOld) SettleAttempt(phash lntypes.Hash,
	pid uint64, settleInfo *paymentsdb.HTLCSettleInfo) (
	*paymentsdb.HTLCAttempt, error) {

	if m.settleAttempt != nil {
		m.settleAttempt <- settleAttemptArgs{settleInfo.Preimage}
	}

	m.Lock()
	defer m.Unlock()

	// Only allow setting attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return nil, paymentsdb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the settle info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return nil, paymentsdb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return nil, paymentsdb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Settle = settleInfo

		// Mark the payment successful on first settled attempt.
		m.successful[phash] = struct{}{}

		return &paymentsdb.HTLCAttempt{
			Settle: settleInfo,
		}, nil
	}

	return nil, fmt.Errorf("pid not found")
}

func (m *mockControlTowerOld) FailAttempt(phash lntypes.Hash, pid uint64,
	failInfo *paymentsdb.HTLCFailInfo) (*paymentsdb.HTLCAttempt, error) {

	if m.failAttempt != nil {
		m.failAttempt <- failAttemptArgs{failInfo}
	}

	m.Lock()
	defer m.Unlock()

	// Only allow failing attempts if the payment is known.
	p, ok := m.payments[phash]
	if !ok {
		return nil, paymentsdb.ErrPaymentNotInitiated
	}

	// Find the attempt with this pid, and set the failure info.
	for i, a := range p.attempts {
		if a.AttemptID != pid {
			continue
		}

		if a.Settle != nil {
			return nil, paymentsdb.ErrAttemptAlreadySettled
		}
		if a.Failure != nil {
			return nil, paymentsdb.ErrAttemptAlreadyFailed
		}

		p.attempts[i].Failure = failInfo

		return &paymentsdb.HTLCAttempt{
			Failure: failInfo,
		}, nil
	}

	return nil, fmt.Errorf("pid not found")
}

func (m *mockControlTowerOld) FailPayment(phash lntypes.Hash,
	reason paymentsdb.FailureReason) error {

	m.Lock()
	defer m.Unlock()

	if m.failPayment != nil {
		m.failPayment <- failPaymentArgs{reason}
	}

	// Payment must be known.
	if _, ok := m.payments[phash]; !ok {
		return paymentsdb.ErrPaymentNotInitiated
	}

	m.failed[phash] = reason

	return nil
}

func (m *mockControlTowerOld) FetchPayment(phash lntypes.Hash) (
	paymentsdb.DBMPPayment, error) {

	m.Lock()
	defer m.Unlock()

	return m.fetchPayment(phash)
}

func (m *mockControlTowerOld) fetchPayment(phash lntypes.Hash) (
	*paymentsdb.MPPayment, error) {

	p, ok := m.payments[phash]
	if !ok {
		return nil, paymentsdb.ErrPaymentNotInitiated
	}

	mp := &paymentsdb.MPPayment{
		Info: &p.info,
	}

	reason, ok := m.failed[phash]
	if ok {
		mp.FailureReason = &reason
	}

	// Return a copy of the current attempts.
	mp.HTLCs = append(mp.HTLCs, p.attempts...)

	if err := mp.SetState(); err != nil {
		return nil, err
	}

	return mp, nil
}

func (m *mockControlTowerOld) FetchInFlightPayments() (
	[]*paymentsdb.MPPayment, error) {

	if m.fetchInFlight != nil {
		m.fetchInFlight <- struct{}{}
	}

	m.Lock()
	defer m.Unlock()

	// In flight are all payments not successful or failed.
	var fl []*paymentsdb.MPPayment
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
	ControlTowerSubscriber, error) {

	return nil, errors.New("not implemented")
}

func (m *mockControlTowerOld) SubscribeAllPayments() (
	ControlTowerSubscriber, error) {

	return nil, errors.New("not implemented")
}

type mockPaymentAttemptDispatcher struct {
	mock.Mock
}

var _ PaymentAttemptDispatcher = (*mockPaymentAttemptDispatcher)(nil)

func (m *mockPaymentAttemptDispatcher) SendHTLC(firstHop lnwire.ShortChannelID,
	pid uint64, htlcAdd *lnwire.UpdateAddHTLC) error {

	args := m.Called(firstHop, pid, htlcAdd)
	return args.Error(0)
}

func (m *mockPaymentAttemptDispatcher) HasAttemptResult(
	attemptID uint64) (bool, error) {

	args := m.Called(attemptID)
	return args.Bool(0), args.Error(1)
}

func (m *mockPaymentAttemptDispatcher) GetAttemptResult(attemptID uint64,
	paymentHash lntypes.Hash, deobfuscator htlcswitch.ErrorDecrypter) (
	<-chan *htlcswitch.PaymentResult, error) {

	args := m.Called(attemptID, paymentHash, deobfuscator)

	resultChan := args.Get(0)
	if resultChan == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(chan *htlcswitch.PaymentResult), args.Error(1)
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
	payment *LightningPayment, firstHopBlob fn.Option[tlv.Blob],
	tlvShaper fn.Option[htlcswitch.AuxTrafficShaper]) (PaymentSession,
	error) {

	args := m.Called(payment, firstHopBlob, tlvShaper)
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

var _ MissionControlQuerier = (*mockMissionControl)(nil)

func (m *mockMissionControl) ReportPaymentFail(
	paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*paymentsdb.FailureReason, error) {

	args := m.Called(paymentID, rt, failureSourceIdx, failure)

	// Type assertion on nil will fail, so we check and return here.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*paymentsdb.FailureReason), args.Error(1)
}

func (m *mockMissionControl) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	args := m.Called(paymentID, rt)
	return args.Error(0)
}

func (m *mockMissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64 {

	args := m.Called(fromNode, toNode, amt, capacity)
	return args.Get(0).(float64)
}

type mockPaymentSession struct {
	mock.Mock
}

var _ PaymentSession = (*mockPaymentSession)(nil)

func (m *mockPaymentSession) RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
	activeShards, height uint32,
	firstHopCustomRecords lnwire.CustomRecords) (*route.Route, error) {

	args := m.Called(
		maxAmt, feeLimit, activeShards, height, firstHopCustomRecords,
	)

	// Type assertion on nil will fail, so we check and return here.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*route.Route), args.Error(1)
}

func (m *mockPaymentSession) UpdateAdditionalEdge(msg *lnwire.ChannelUpdate1,
	pubKey *btcec.PublicKey, policy *models.CachedEdgePolicy) bool {

	args := m.Called(msg, pubKey, policy)
	return args.Bool(0)
}

func (m *mockPaymentSession) GetAdditionalEdgePolicy(pubKey *btcec.PublicKey,
	channelID uint64) *models.CachedEdgePolicy {

	args := m.Called(pubKey, channelID)
	return args.Get(0).(*models.CachedEdgePolicy)
}

type mockControlTower struct {
	mock.Mock
}

var _ ControlTower = (*mockControlTower)(nil)

func (m *mockControlTower) InitPayment(phash lntypes.Hash,
	c *paymentsdb.PaymentCreationInfo) error {

	args := m.Called(phash, c)
	return args.Error(0)
}

func (m *mockControlTower) DeleteFailedAttempts(phash lntypes.Hash) error {
	args := m.Called(phash)
	return args.Error(0)
}

func (m *mockControlTower) RegisterAttempt(phash lntypes.Hash,
	a *paymentsdb.HTLCAttemptInfo) error {

	args := m.Called(phash, a)
	return args.Error(0)
}

func (m *mockControlTower) SettleAttempt(phash lntypes.Hash,
	pid uint64, settleInfo *paymentsdb.HTLCSettleInfo) (
	*paymentsdb.HTLCAttempt, error) {

	args := m.Called(phash, pid, settleInfo)

	attempt := args.Get(0)
	if attempt == nil {
		return nil, args.Error(1)
	}

	return attempt.(*paymentsdb.HTLCAttempt), args.Error(1)
}

func (m *mockControlTower) FailAttempt(phash lntypes.Hash, pid uint64,
	failInfo *paymentsdb.HTLCFailInfo) (*paymentsdb.HTLCAttempt, error) {

	args := m.Called(phash, pid, failInfo)

	attempt := args.Get(0)
	if attempt == nil {
		return nil, args.Error(1)
	}

	return attempt.(*paymentsdb.HTLCAttempt), args.Error(1)
}

func (m *mockControlTower) FailPayment(phash lntypes.Hash,
	reason paymentsdb.FailureReason) error {

	args := m.Called(phash, reason)
	return args.Error(0)
}

func (m *mockControlTower) FetchPayment(phash lntypes.Hash) (
	paymentsdb.DBMPPayment, error) {

	args := m.Called(phash)

	// Type assertion on nil will fail, so we check and return here.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	payment := args.Get(0).(*mockMPPayment)
	return payment, args.Error(1)
}

func (m *mockControlTower) FetchInFlightPayments() (
	[]*paymentsdb.MPPayment, error) {

	args := m.Called()
	return args.Get(0).([]*paymentsdb.MPPayment), args.Error(1)
}

func (m *mockControlTower) SubscribePayment(paymentHash lntypes.Hash) (
	ControlTowerSubscriber, error) {

	args := m.Called(paymentHash)
	return args.Get(0).(ControlTowerSubscriber), args.Error(1)
}

func (m *mockControlTower) SubscribeAllPayments() (
	ControlTowerSubscriber, error) {

	args := m.Called()
	return args.Get(0).(ControlTowerSubscriber), args.Error(1)
}

type mockMPPayment struct {
	mock.Mock
}

var _ paymentsdb.DBMPPayment = (*mockMPPayment)(nil)

func (m *mockMPPayment) GetState() *paymentsdb.MPPaymentState {
	args := m.Called()
	return args.Get(0).(*paymentsdb.MPPaymentState)
}

func (m *mockMPPayment) GetStatus() paymentsdb.PaymentStatus {
	args := m.Called()
	return args.Get(0).(paymentsdb.PaymentStatus)
}

func (m *mockMPPayment) Terminated() bool {
	args := m.Called()

	return args.Bool(0)
}

func (m *mockMPPayment) NeedWaitAttempts() (bool, error) {
	args := m.Called()
	return args.Bool(0), args.Error(1)
}

func (m *mockMPPayment) GetHTLCs() []paymentsdb.HTLCAttempt {
	args := m.Called()
	return args.Get(0).([]paymentsdb.HTLCAttempt)
}

func (m *mockMPPayment) InFlightHTLCs() []paymentsdb.HTLCAttempt {
	args := m.Called()
	return args.Get(0).([]paymentsdb.HTLCAttempt)
}

func (m *mockMPPayment) AllowMoreAttempts() (bool, error) {
	args := m.Called()
	return args.Bool(0), args.Error(1)
}

func (m *mockMPPayment) TerminalInfo() (*paymentsdb.HTLCAttempt,
	*paymentsdb.FailureReason) {

	args := m.Called()

	var (
		settleInfo  *paymentsdb.HTLCAttempt
		failureInfo *paymentsdb.FailureReason
	)

	settle := args.Get(0)
	if settle != nil {
		settleInfo = settle.(*paymentsdb.HTLCAttempt)
	}

	reason := args.Get(1)
	if reason != nil {
		failureInfo = reason.(*paymentsdb.FailureReason)
	}

	return settleInfo, failureInfo
}

type mockLink struct {
	htlcswitch.ChannelLink
	bandwidth         lnwire.MilliSatoshi
	mayAddOutgoingErr error
	ineligible        bool
}

// Bandwidth returns the bandwidth the mock was configured with.
func (m *mockLink) Bandwidth() lnwire.MilliSatoshi {
	return m.bandwidth
}

// AuxBandwidth returns the bandwidth that can be used for a channel,
// expressed in milli-satoshi. This might be different from the regular
// BTC bandwidth for custom channels. This will always return fn.None()
// for a regular (non-custom) channel.
func (m *mockLink) AuxBandwidth(lnwire.MilliSatoshi, lnwire.ShortChannelID,
	fn.Option[tlv.Blob],
	htlcswitch.AuxTrafficShaper) fn.Result[htlcswitch.OptionalBandwidth] {

	return fn.Ok(htlcswitch.OptionalBandwidth{})
}

// EligibleToForward returns the mock's configured eligibility.
func (m *mockLink) EligibleToForward() bool {
	return !m.ineligible
}

// MayAddOutgoingHtlc returns the error configured in our mock.
func (m *mockLink) MayAddOutgoingHtlc(_ lnwire.MilliSatoshi) error {
	return m.mayAddOutgoingErr
}

func (m *mockLink) FundingCustomBlob() fn.Option[tlv.Blob] {
	return fn.None[tlv.Blob]()
}

func (m *mockLink) CommitmentCustomBlob() fn.Option[tlv.Blob] {
	return fn.None[tlv.Blob]()
}

type mockShardTracker struct {
	mock.Mock
}

var _ shards.ShardTracker = (*mockShardTracker)(nil)

func (m *mockShardTracker) NewShard(attemptID uint64,
	lastShard bool) (shards.PaymentShard, error) {

	args := m.Called(attemptID, lastShard)

	shard := args.Get(0)
	if shard == nil {
		return nil, args.Error(1)
	}

	return shard.(shards.PaymentShard), args.Error(1)
}

func (m *mockShardTracker) GetHash(attemptID uint64) (lntypes.Hash, error) {
	args := m.Called(attemptID)
	return args.Get(0).(lntypes.Hash), args.Error(1)
}

func (m *mockShardTracker) CancelShard(attemptID uint64) error {
	args := m.Called(attemptID)
	return args.Error(0)
}

type mockShard struct {
	mock.Mock
}

var _ shards.PaymentShard = (*mockShard)(nil)

// Hash returns the hash used for the HTLC representing this shard.
func (m *mockShard) Hash() lntypes.Hash {
	args := m.Called()
	return args.Get(0).(lntypes.Hash)
}

// MPP returns any extra MPP records that should be set for the final
// hop on the route used by this shard.
func (m *mockShard) MPP() *record.MPP {
	args := m.Called()

	r := args.Get(0)
	if r == nil {
		return nil
	}

	return r.(*record.MPP)
}

// AMP returns any extra AMP records that should be set for the final
// hop on the route used by this shard.
func (m *mockShard) AMP() *record.AMP {
	args := m.Called()

	r := args.Get(0)
	if r == nil {
		return nil
	}

	return r.(*record.AMP)
}
