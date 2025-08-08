package routing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DefaultPenaltyHalfLife is the default half-life duration. The
	// half-life duration defines after how much time a penalized node or
	// channel is back at 50% probability.
	DefaultPenaltyHalfLife = time.Hour

	// minSecondChanceInterval is the minimum time required between
	// second-chance failures.
	//
	// If nodes return a channel policy related failure, they may get a
	// second chance to forward the payment. It could be that the channel
	// policy that we are aware of is not up to date. This is especially
	// important in case of mobile apps that are mostly offline.
	//
	// However, we don't want to give nodes the option to endlessly return
	// new channel updates so that we are kept busy trying to route through
	// that node until the payment loop times out.
	//
	// Therefore we only grant a second chance to a node if the previous
	// second chance is sufficiently long ago. This is what
	// minSecondChanceInterval defines. If a second policy failure comes in
	// within that interval, we will apply a penalty.
	//
	// Second chances granted are tracked on the level of node pairs. This
	// means that if a node has multiple channels to the same peer, they
	// will only get a single second chance to route to that peer again.
	// Nodes forward non-strict, so it isn't necessary to apply a less
	// restrictive channel level tracking scheme here.
	minSecondChanceInterval = time.Minute

	// DefaultMaxMcHistory is the default maximum history size.
	DefaultMaxMcHistory = 1000

	// DefaultMcFlushInterval is the default interval we use to flush MC state
	// to the database.
	DefaultMcFlushInterval = time.Second

	// prevSuccessProbability is the assumed probability for node pairs that
	// successfully relayed the previous attempt.
	prevSuccessProbability = 0.95

	// DefaultAprioriWeight is the default a priori weight. See
	// MissionControlConfig for further explanation.
	DefaultAprioriWeight = 0.5

	// DefaultMinFailureRelaxInterval is the default minimum time that must
	// have passed since the previously recorded failure before the failure
	// amount may be raised.
	DefaultMinFailureRelaxInterval = time.Minute

	// DefaultFeeEstimationTimeout is the default value for
	// FeeEstimationTimeout. It defines the maximum duration that the
	// probing fee estimation is allowed to take.
	DefaultFeeEstimationTimeout = time.Minute

	// DefaultMissionControlNamespace is the name of the default mission
	// control name space. This is used as the sub-bucket key within the
	// top level DB bucket to store mission control results.
	DefaultMissionControlNamespace = "default"
)

var (
	// ErrInvalidMcHistory is returned if we get a negative mission control
	// history count.
	ErrInvalidMcHistory = errors.New("mission control history must be " +
		">= 0")

	// ErrInvalidFailureInterval is returned if we get an invalid failure
	// interval.
	ErrInvalidFailureInterval = errors.New("failure interval must be >= 0")
)

// NodeResults contains previous results from a node to its peers.
type NodeResults map[route.Vertex]TimedPairResult

// mcConfig holds various config members that will be required by all
// MissionControl instances and will be the same regardless of namespace.
type mcConfig struct {
	// clock is a time source used by mission control.
	clock clock.Clock

	// selfNode is our pubkey.
	selfNode route.Vertex
}

// MissionControl contains state which summarizes the past attempts of HTLC
// routing by external callers when sending payments throughout the network. It
// acts as a shared memory during routing attempts with the goal to optimize the
// payment attempt success rate.
//
// Failed payment attempts are reported to mission control. These reports are
// used to track the time of the last node or channel level failure. The time
// since the last failure is used to estimate a success probability that is fed
// into the path finding process for subsequent payment attempts.
type MissionControl struct {
	cfg *mcConfig

	// state is the internal mission control state that is input for
	// probability estimation.
	state *missionControlState

	store *missionControlStore

	// estimator is the probability estimator that is used with the payment
	// results that mission control collects.
	estimator Estimator

	// onConfigUpdate is a function that is called whenever the
	// mission control state is updated.
	onConfigUpdate fn.Option[func(cfg *MissionControlConfig)]

	log btclog.Logger

	mu sync.Mutex
}

// MissionController manages MissionControl instances in various namespaces.
type MissionController struct {
	db           kvdb.Backend
	cfg          *mcConfig
	defaultMCCfg *MissionControlConfig

	mc map[string]*MissionControl
	mu sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// GetNamespacedStore returns the MissionControl in the given namespace. If one
// does not yet exist, then it is initialised.
func (m *MissionController) GetNamespacedStore(ns string) (*MissionControl,
	error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if mc, ok := m.mc[ns]; ok {
		return mc, nil
	}

	return m.initMissionControl(ns)
}

// ListNamespaces returns a list of the namespaces that the MissionController
// is aware of.
func (m *MissionController) ListNamespaces() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	namespaces := make([]string, 0, len(m.mc))
	for ns := range m.mc {
		namespaces = append(namespaces, ns)
	}

	return namespaces
}

// MissionControlConfig defines parameters that control mission control
// behaviour.
type MissionControlConfig struct {
	// Estimator gives probability estimates for node pairs.
	Estimator Estimator

	// OnConfigUpdate is function that is called whenever the
	// mission control state is updated.
	OnConfigUpdate fn.Option[func(cfg *MissionControlConfig)]

	// MaxMcHistory defines the maximum number of payment results that are
	// held on disk.
	MaxMcHistory int

	// McFlushInterval defines the ticker interval when we flush the
	// accumulated state to the DB.
	McFlushInterval time.Duration

	// MinFailureRelaxInterval is the minimum time that must have passed
	// since the previously recorded failure before the failure amount may
	// be raised.
	MinFailureRelaxInterval time.Duration
}

func (c *MissionControlConfig) validate() error {
	if c.MaxMcHistory < 0 {
		return ErrInvalidMcHistory
	}

	if c.MinFailureRelaxInterval < 0 {
		return ErrInvalidFailureInterval
	}

	return nil
}

// String returns a string representation of a mission control config.
func (c *MissionControlConfig) String() string {
	return fmt.Sprintf("maximum history: %v, minimum failure relax "+
		"interval: %v", c.MaxMcHistory, c.MinFailureRelaxInterval)
}

// TimedPairResult describes a timestamped pair result.
type TimedPairResult struct {
	// FailTime is the time of the last failure.
	FailTime time.Time

	// FailAmt is the amount of the last failure. This amount may be pushed
	// up if a later success is higher than the last failed amount.
	FailAmt lnwire.MilliSatoshi

	// SuccessTime is the time of the last success.
	SuccessTime time.Time

	// SuccessAmt is the highest amount that successfully forwarded. This
	// isn't necessarily the last success amount. The value of this field
	// may also be pushed down if a later failure is lower than the highest
	// success amount. Because of this, SuccessAmt may not match
	// SuccessTime.
	SuccessAmt lnwire.MilliSatoshi
}

// MissionControlSnapshot contains a snapshot of the current state of mission
// control.
type MissionControlSnapshot struct {
	// Pairs is a list of channels for which specific information is
	// logged.
	Pairs []MissionControlPairSnapshot
}

// MissionControlPairSnapshot contains a snapshot of the current node pair
// state in mission control.
type MissionControlPairSnapshot struct {
	// Pair is the node pair of which the state is described.
	Pair DirectedNodePair

	// TimedPairResult contains the data for this pair.
	TimedPairResult
}

// paymentResult is the information that becomes available when a payment
// attempt completes.
type paymentResult struct {
	id        uint64
	timeFwd   tlv.RecordT[tlv.TlvType0, uint64]
	timeReply tlv.RecordT[tlv.TlvType1, uint64]
	route     tlv.RecordT[tlv.TlvType2, mcRoute]

	// failure holds information related to the failure of a payment. The
	// presence of this record indicates a payment failure. The absence of
	// this record indicates a successful payment.
	failure tlv.OptionalRecordT[tlv.TlvType3, paymentFailure]
}

// newPaymentResult constructs a new paymentResult.
func newPaymentResult(id uint64, rt *mcRoute, timeFwd, timeReply time.Time,
	failure fn.Option[paymentFailure]) *paymentResult {

	result := &paymentResult{
		id: id,
		timeFwd: tlv.NewPrimitiveRecord[tlv.TlvType0](
			uint64(timeFwd.UnixNano()),
		),
		timeReply: tlv.NewPrimitiveRecord[tlv.TlvType1](
			uint64(timeReply.UnixNano()),
		),
		route: tlv.NewRecordT[tlv.TlvType2](*rt),
	}

	failure.WhenSome(
		func(f paymentFailure) {
			result.failure = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType3](f),
			)
		},
	)

	return result
}

// NewMissionController returns a new instance of MissionController.
func NewMissionController(db kvdb.Backend, self route.Vertex,
	cfg *MissionControlConfig) (*MissionController, error) {

	log.Debugf("Instantiating mission control with config: %v, %v", cfg,
		cfg.Estimator)

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	mcCfg := &mcConfig{
		clock:    clock.NewDefaultClock(),
		selfNode: self,
	}

	mgr := &MissionController{
		db:           db,
		defaultMCCfg: cfg,
		cfg:          mcCfg,
		mc:           make(map[string]*MissionControl),
	}

	if err := mgr.loadMissionControls(); err != nil {
		return nil, err
	}

	for _, mc := range mgr.mc {
		if err := mc.init(); err != nil {
			return nil, err
		}
	}

	return mgr, nil
}

// loadMissionControls initialises a MissionControl in the default namespace if
// one does not yet exist. It then initialises a MissionControl for all other
// namespaces found in the DB.
//
// NOTE: this should only be called once during MissionController construction.
func (m *MissionController) loadMissionControls() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Always initialise the default namespace.
	_, err := m.initMissionControl(DefaultMissionControlNamespace)
	if err != nil {
		return err
	}

	namespaces := make(map[string]struct{})
	err = m.db.View(func(tx walletdb.ReadTx) error {
		mcStoreBkt := tx.ReadBucket(resultsKey)
		if mcStoreBkt == nil {
			return fmt.Errorf("top level mission control bucket " +
				"not found")
		}

		// Iterate through all the keys in the bucket and collect the
		// namespaces.
		return mcStoreBkt.ForEach(func(k, _ []byte) error {
			// We've already initialised the default namespace so
			// we can skip it.
			if string(k) == DefaultMissionControlNamespace {
				return nil
			}

			namespaces[string(k)] = struct{}{}

			return nil
		})
	}, func() {})
	if err != nil {
		return err
	}

	// Now, iterate through all the namespaces and initialise them.
	for ns := range namespaces {
		_, err = m.initMissionControl(ns)
		if err != nil {
			return err
		}
	}

	return nil
}

// initMissionControl creates a new MissionControl instance with the given
// namespace if one does not yet exist.
//
// NOTE: the MissionController's mutex must be held before calling this method.
func (m *MissionController) initMissionControl(namespace string) (
	*MissionControl, error) {

	// If a mission control with this namespace has already been initialised
	// then there is nothing left to do.
	if mc, ok := m.mc[namespace]; ok {
		return mc, nil
	}

	cfg := m.defaultMCCfg

	store, err := newMissionControlStore(
		newNamespacedDB(m.db, namespace), cfg.MaxMcHistory,
		cfg.McFlushInterval,
	)
	if err != nil {
		return nil, err
	}

	mc := &MissionControl{
		cfg: m.cfg,
		state: newMissionControlState(
			cfg.MinFailureRelaxInterval,
		),
		store:          store,
		estimator:      cfg.Estimator,
		log:            log.WithPrefix(fmt.Sprintf("[%s]:", namespace)),
		onConfigUpdate: cfg.OnConfigUpdate,
	}

	m.mc[namespace] = mc

	return mc, nil
}

// RunStoreTickers runs the mission controller store's tickers.
func (m *MissionController) RunStoreTickers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, mc := range m.mc {
		mc.store.run()
	}
}

// StopStoreTickers stops the mission control store's tickers.
func (m *MissionController) StopStoreTickers() {
	log.Debug("Stopping mission control store ticker")
	defer log.Debug("Mission control store ticker stopped")

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, mc := range m.mc {
		mc.store.stop()
	}
}

// init initializes mission control with historical data.
func (m *MissionControl) init() error {
	m.log.Debugf("Mission control state reconstruction started")

	m.mu.Lock()
	defer m.mu.Unlock()

	start := time.Now()

	results, err := m.store.fetchAll()
	if err != nil {
		return err
	}

	for _, result := range results {
		_ = m.applyPaymentResult(result)
	}

	m.log.Debugf("Mission control state reconstruction finished: "+
		"n=%v, time=%v", len(results), time.Since(start))

	return nil
}

// GetConfig returns the config that mission control is currently configured
// with. All fields are copied by value, so we do not need to worry about
// mutation.
func (m *MissionControl) GetConfig() *MissionControlConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return &MissionControlConfig{
		Estimator:               m.estimator,
		MaxMcHistory:            m.store.maxRecords,
		McFlushInterval:         m.store.flushInterval,
		MinFailureRelaxInterval: m.state.minFailureRelaxInterval,
	}
}

// SetConfig validates the config provided and updates mission control's config
// if it is valid.
func (m *MissionControl) SetConfig(cfg *MissionControlConfig) error {
	if cfg == nil {
		return errors.New("nil mission control config")
	}

	if err := cfg.validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Infof("Active mission control cfg: %v, estimator: %v", cfg,
		cfg.Estimator)

	m.store.maxRecords = cfg.MaxMcHistory
	m.state.minFailureRelaxInterval = cfg.MinFailureRelaxInterval
	m.estimator = cfg.Estimator

	// Execute the callback function if it is set.
	m.onConfigUpdate.WhenSome(func(f func(cfg *MissionControlConfig)) {
		f(cfg)
	})

	return nil
}

// ResetHistory resets the history of MissionControl returning it to a state as
// if no payment attempts have been made.
func (m *MissionControl) ResetHistory() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.store.clear(); err != nil {
		return err
	}

	m.state.resetHistory()

	m.log.Debugf("Mission control history cleared")

	return nil
}

// GetProbability is expected to return the success probability of a payment
// from fromNode along edge.
func (m *MissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64 {

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.cfg.clock.Now()
	results, _ := m.state.getLastPairResult(fromNode)

	// Use a distinct probability estimation function for local channels.
	if fromNode == m.cfg.selfNode {
		return m.estimator.LocalPairProbability(now, results, toNode)
	}

	return m.estimator.PairProbability(
		now, results, toNode, amt, capacity,
	)
}

// GetHistorySnapshot takes a snapshot from the current mission control state
// and actual probability estimates.
func (m *MissionControl) GetHistorySnapshot() *MissionControlSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Debugf("Requesting history snapshot from mission control")

	return m.state.getSnapshot()
}

// ImportHistory imports the set of mission control results provided to our
// in-memory state. These results are not persisted, so will not survive
// restarts.
func (m *MissionControl) ImportHistory(history *MissionControlSnapshot,
	force bool) error {

	if history == nil {
		return errors.New("cannot import nil history")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Infof("Importing history snapshot with %v pairs to mission "+
		"control", len(history.Pairs))

	imported := m.state.importSnapshot(history, force)

	m.log.Infof("Imported %v results to mission control", imported)

	return nil
}

// GetPairHistorySnapshot returns the stored history for a given node pair.
func (m *MissionControl) GetPairHistorySnapshot(
	fromNode, toNode route.Vertex) TimedPairResult {

	m.mu.Lock()
	defer m.mu.Unlock()

	results, ok := m.state.getLastPairResult(fromNode)
	if !ok {
		return TimedPairResult{}
	}

	result, ok := results[toNode]
	if !ok {
		return TimedPairResult{}
	}

	return result
}

// ReportPaymentFail reports a failed payment to mission control as input for
// future probability estimates. The failureSourceIdx argument indicates the
// failure source. If it is nil, the failure source is unknown. This function
// returns a reason if this failure is a final failure. In that case no further
// payment attempts need to be made.
func (m *MissionControl) ReportPaymentFail(paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*paymentsdb.FailureReason, error) {

	timestamp := m.cfg.clock.Now()

	result := newPaymentResult(
		paymentID, extractMCRoute(rt), timestamp, timestamp,
		fn.Some(newPaymentFailure(failureSourceIdx, failure)),
	)

	return m.processPaymentResult(result)
}

// ReportPaymentSuccess reports a successful payment to mission control as input
// for future probability estimates.
func (m *MissionControl) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	timestamp := m.cfg.clock.Now()

	result := newPaymentResult(
		paymentID, extractMCRoute(rt), timestamp, timestamp,
		fn.None[paymentFailure](),
	)

	_, err := m.processPaymentResult(result)

	return err
}

// processPaymentResult stores a payment result in the mission control store and
// updates mission control's in-memory state.
func (m *MissionControl) processPaymentResult(result *paymentResult) (
	*paymentsdb.FailureReason, error) {

	// Store complete result in database.
	m.store.AddResult(result)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Apply result to update mission control state.
	reason := m.applyPaymentResult(result)

	return reason, nil
}

// applyPaymentResult applies a payment result as input for future probability
// estimates. It returns a bool indicating whether this error is a final error
// and no further payment attempts need to be made.
func (m *MissionControl) applyPaymentResult(
	result *paymentResult) *paymentsdb.FailureReason {

	// Interpret result.
	i := interpretResult(&result.route.Val, result.failure.ValOpt())

	if i.policyFailure != nil {
		if m.state.requestSecondChance(
			time.Unix(0, int64(result.timeReply.Val)),
			i.policyFailure.From, i.policyFailure.To,
		) {
			return nil
		}
	}

	// If there is a node-level failure, record a failure for every tried
	// connection of that node. A node-level failure can be considered as a
	// failure that would have occurred with any of the node's channels.
	//
	// Ideally we'd also record the failure for the untried connections of
	// the node. Unfortunately this would require access to the graph and
	// adding this dependency and db calls does not outweigh the benefits.
	//
	// Untried connections will fall back to the node probability. After the
	// call to setAllPairResult below, the node probability will be equal to
	// the probability of the tried channels except that the a priori
	// probability is mixed in too. This effect is controlled by the
	// aprioriWeight parameter. If that parameter isn't set to an extreme
	// and there are a few known connections, there shouldn't be much of a
	// difference. The largest difference occurs when aprioriWeight is 1. In
	// that case, a node-level failure would not be applied to untried
	// channels.
	if i.nodeFailure != nil {
		m.log.Debugf("Reporting node failure to Mission Control: "+
			"node=%v", *i.nodeFailure)

		m.state.setAllFail(
			*i.nodeFailure,
			time.Unix(0, int64(result.timeReply.Val)),
		)
	}

	for pair, pairResult := range i.pairResults {
		pairResult := pairResult

		if pairResult.success {
			m.log.Debugf("Reporting pair success to Mission "+
				"Control: pair=%v, amt=%v",
				pair, pairResult.amt)
		} else {
			m.log.Debugf("Reporting pair failure to Mission "+
				"Control: pair=%v, amt=%v",
				pair, pairResult.amt)
		}

		m.state.setLastPairResult(
			pair.From, pair.To,
			time.Unix(0, int64(result.timeReply.Val)), &pairResult,
			false,
		)
	}

	return i.finalFailureReason
}

// namespacedDB is an implementation of the missionControlDB that gives a user
// of the interface access to a namespaced bucket within the top level mission
// control bucket.
type namespacedDB struct {
	topLevelBucketKey []byte
	namespace         []byte
	db                kvdb.Backend
}

// A compile-time check to ensure that namespacedDB implements missionControlDB.
var _ missionControlDB = (*namespacedDB)(nil)

// newDefaultNamespacedStore creates an instance of namespaceDB that uses the
// default namespace.
func newDefaultNamespacedStore(db kvdb.Backend) missionControlDB {
	return newNamespacedDB(db, DefaultMissionControlNamespace)
}

// newNamespacedDB creates a new instance of missionControlDB where the DB will
// have access to a namespaced bucket within the top level mission control
// bucket.
func newNamespacedDB(db kvdb.Backend, namespace string) missionControlDB {
	return &namespacedDB{
		db:                db,
		namespace:         []byte(namespace),
		topLevelBucketKey: resultsKey,
	}
}

// update can be used to perform reads and writes on the given bucket.
//
// NOTE: this is part of the missionControlDB interface.
func (n *namespacedDB) update(f func(bkt walletdb.ReadWriteBucket) error,
	reset func()) error {

	return n.db.Update(func(tx kvdb.RwTx) error {
		mcStoreBkt, err := tx.CreateTopLevelBucket(n.topLevelBucketKey)
		if err != nil {
			return fmt.Errorf("cannot create top level mission "+
				"control bucket: %w", err)
		}

		namespacedBkt, err := mcStoreBkt.CreateBucketIfNotExists(
			n.namespace,
		)
		if err != nil {
			return fmt.Errorf("cannot create namespaced bucket "+
				"(%s) in mission control store: %w",
				n.namespace, err)
		}

		return f(namespacedBkt)
	}, reset)
}

// view can be used to perform reads on the given bucket.
//
// NOTE: this is part of the missionControlDB interface.
func (n *namespacedDB) view(f func(bkt walletdb.ReadBucket) error,
	reset func()) error {

	return n.db.View(func(tx kvdb.RTx) error {
		mcStoreBkt := tx.ReadBucket(n.topLevelBucketKey)
		if mcStoreBkt == nil {
			return fmt.Errorf("top level mission control bucket " +
				"not found")
		}

		namespacedBkt := mcStoreBkt.NestedReadBucket(n.namespace)
		if namespacedBkt == nil {
			return fmt.Errorf("namespaced bucket (%s) not found "+
				"in mission control store", n.namespace)
		}

		return f(namespacedBkt)
	}, reset)
}

// purge will delete all the contents in the namespace.
//
// NOTE: this is part of the missionControlDB interface.
func (n *namespacedDB) purge() error {
	return n.db.Update(func(tx kvdb.RwTx) error {
		mcStoreBkt := tx.ReadWriteBucket(n.topLevelBucketKey)
		if mcStoreBkt == nil {
			return nil
		}

		err := mcStoreBkt.DeleteNestedBucket(n.namespace)
		if err != nil {
			return err
		}

		_, err = mcStoreBkt.CreateBucket(n.namespace)

		return err
	}, func() {})
}

// newPaymentFailure constructs a new paymentFailure struct. If the source
// index is nil, then an empty paymentFailure is returned. This represents a
// failure with unknown details. Otherwise, the index and failure message are
// used to populate the info field of the paymentFailure.
func newPaymentFailure(sourceIdx *int,
	failureMsg lnwire.FailureMessage) paymentFailure {

	// If we can't identify a failure source, we also won't have a decrypted
	// failure message. In this case we return an empty payment failure.
	if sourceIdx == nil {
		return paymentFailure{}
	}

	info := paymentFailure{
		sourceIdx: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				uint8(*sourceIdx),
			),
		),
	}

	if failureMsg != nil {
		info.msg = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType1](
				failureMessage{failureMsg},
			),
		)
	}

	return info
}

// paymentFailure holds additional information about a payment failure.
type paymentFailure struct {
	// sourceIdx is the hop the error was reported from. In order to be able
	// to decrypt the error message, we need to know the source, which is
	// why an error message can only be present if the source is known.
	sourceIdx tlv.OptionalRecordT[tlv.TlvType0, uint8]

	// msg is the error why a payment failed. If we identify the failure of
	// a certain hop at the above index, but aren't able to decode the
	// failure message we indicate this by not setting this field.
	msg tlv.OptionalRecordT[tlv.TlvType1, failureMessage]
}

// Record returns a TLV record that can be used to encode/decode a
// paymentFailure to/from a TLV stream.
func (r *paymentFailure) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodePaymentFailure(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodePaymentFailure,
		decodePaymentFailure,
	)
}

func encodePaymentFailure(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*paymentFailure); ok {
		var recordProducers []tlv.RecordProducer

		v.sourceIdx.WhenSome(
			func(r tlv.RecordT[tlv.TlvType0, uint8]) {
				recordProducers = append(
					recordProducers, &r,
				)
			},
		)

		v.msg.WhenSome(
			func(r tlv.RecordT[tlv.TlvType1, failureMessage]) {
				recordProducers = append(
					recordProducers, &r,
				)
			},
		)

		return lnwire.EncodeRecordsTo(
			w, lnwire.ProduceRecordsSorted(
				recordProducers...,
			),
		)
	}

	return tlv.NewTypeForEncodingErr(val, "routing.paymentFailure")
}

func decodePaymentFailure(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*paymentFailure); ok {
		var h paymentFailure

		sourceIdx := tlv.ZeroRecordT[tlv.TlvType0, uint8]()
		msg := tlv.ZeroRecordT[tlv.TlvType1, failureMessage]()

		typeMap, err := lnwire.DecodeRecords(
			r,
			lnwire.ProduceRecordsSorted(&sourceIdx, &msg)...,
		)

		if err != nil {
			return err
		}

		if _, ok := typeMap[h.sourceIdx.TlvType()]; ok {
			h.sourceIdx = tlv.SomeRecordT(sourceIdx)
		}

		if _, ok := typeMap[h.msg.TlvType()]; ok {
			h.msg = tlv.SomeRecordT(msg)
		}

		*v = h

		return nil
	}

	return tlv.NewTypeForDecodingErr(
		val, "routing.paymentFailure", l, l,
	)
}
