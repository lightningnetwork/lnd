package chainntnfs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const (
	// asyncStartHeight is the TxNotifier's starting chain height for every
	// run of the asynchronous model.
	asyncStartHeight = uint32(100)

	// asyncReorgLimit is the reorg safety limit passed to the TxNotifier.
	// It is small so that requests mature, and details fall out of the
	// reorg window, within the bounded chain length.
	asyncReorgLimit = uint32(4)

	// asyncMinHint is the lowest height hint a subscriber can draw. It is
	// below the starting height so that hints can precede every block the
	// model mines.
	asyncMinHint = uint32(95)

	// asyncNumRequests is the number of independent requests. Each has
	// both a confirmation target and a spend target.
	asyncNumRequests = 2

	// asyncSlots is the number of subscriber slots per request and kind.
	asyncSlots = 2

	// asyncMaxBlocks bounds the number of blocks connected in a run.
	asyncMaxBlocks = 20
)

// asyncKind distinguishes confirmation and spend subscriptions.
type asyncKind int

const (
	// asyncConf identifies a confirmation subscription.
	asyncConf asyncKind = iota

	// asyncSpend identifies a spend subscription.
	asyncSpend
)

// String returns a readable name for the kind.
func (k asyncKind) String() string {
	if k == asyncConf {
		return "conf"
	}

	return "spend"
}

// asyncSubscriber is one subscriber slot, along with what the oracle knows
// about the notifications it has received.
type asyncSubscriber struct {
	// active is true while the slot holds a registration that has been
	// neither canceled nor retired by a Done notification.
	active bool

	// hint is the height hint the subscriber registered with. It can be
	// later than the event it watches for, in which case the notifier
	// owes this subscriber nothing.
	hint uint32

	// numConfs is the confirmation depth of a confirmation subscriber.
	numConfs uint32

	// confEvent is the event of a confirmation subscriber.
	confEvent *chainntnfs.ConfirmationEvent

	// spendEvent is the event of a spend subscriber.
	spendEvent *chainntnfs.SpendEvent

	// delivered is true once the subscriber has received the event for
	// the target's current inclusion in the chain.
	delivered bool

	// reorgs counts the disconnections of the target's block since the
	// subscriber registered, bounding the reorg notifications it may
	// receive.
	reorgs int
}

// asyncRequest is one confirmation target and one spend target, together
// with where they are currently mined and their subscriber slots.
type asyncRequest struct {
	// confTx is the transaction confirmation subscribers watch for.
	confTx *wire.MsgTx

	// confScript is the output script of confTx.
	confScript []byte

	// confHeight is the height at which confTx is mined, or zero.
	confHeight uint32

	// confBlock is the block containing confTx while it is mined.
	confBlock *btcutil.Block

	// outpoint is the outpoint spend subscribers watch.
	outpoint wire.OutPoint

	// spendScript is the output script of outpoint.
	spendScript []byte

	// spendTx is the transaction that spends outpoint.
	spendTx *wire.MsgTx

	// spendHeight is the height at which spendTx is mined, or zero.
	spendHeight uint32

	// subs holds the subscriber slots of each kind.
	subs [2][asyncSlots]asyncSubscriber
}

// minedHeight returns the height at which the target of the given kind is
// mined, or zero if it is not in the chain.
func (r *asyncRequest) minedHeight(kind asyncKind) uint32 {
	if kind == asyncConf {
		return r.confHeight
	}

	return r.spendHeight
}

// asyncScan is a historical scan the notifier dispatched and the model has
// not yet completed or failed.
type asyncScan struct {
	kind    asyncKind
	request int
	conf    *chainntnfs.HistoricalConfDispatch
	spend   *chainntnfs.HistoricalSpendDispatch
}

// bounds returns the inclusive range the scan covers.
func (s asyncScan) bounds() (uint32, uint32) {
	if s.kind == asyncConf {
		return s.conf.StartHeight, s.conf.EndHeight
	}

	return s.spend.StartHeight, s.spend.EndHeight
}

// asyncBlock records what one connected block carried, so Disconnect can undo
// it. request is -1 for an empty block.
type asyncBlock struct {
	kind    asyncKind
	request int
}

// subRef identifies one subscriber slot.
type subRef struct {
	kind    asyncKind
	request int
	slot    int
}

// asyncModel drives a TxNotifier with historical scans that complete, fail,
// and report progress in any order relative to registrations, blocks, bounded
// reorgs, and restarts. Subscribers draw their own height hints, so a later
// subscriber can have an earlier hint, and a hint can be later than the event
// itself. Rather than predicting each notification, the oracle checks
// invariants after every action: no subscriber receives an event that is not
// in the chain, a subscriber whose hint precedes its event is eventually
// notified, and the hint cache never lets such a subscriber skip its event if
// it registered first after a restart.
//
// The model does not cancel subscriptions, re-register a request once its set
// has matured, or report Neutrino progress inside the reorg window. Those
// paths are covered by a follow-up that reworks request set lifetimes.
type asyncModel struct {
	n             *chainntnfs.TxNotifier
	cache         *mockHintCache
	currentHeight uint32

	// maxTip is the highest tip reached. Reorgs never disconnect a block
	// more than asyncReorgLimit blocks below it, which is the depth the
	// notifier assumes is final.
	maxTip uint32

	requests [asyncNumRequests]asyncRequest
	blocks   []asyncBlock

	// scans are the dispatched historical scans still outstanding.
	scans []asyncScan

	// failed records requests with a failed scan in the current notifier
	// instance. Their subscribers are not owed a historical event until a
	// restart scans again.
	failed [2][asyncNumRequests]bool

	// matured records requests whose set was retired by a Done
	// notification. They take no new registrations, since a scan still
	// outstanding from the retired set would report to the new one.
	matured [2][asyncNumRequests]bool
}

// newAsyncModel creates a model with a fresh notifier and hint cache.
func newAsyncModel() *asyncModel {
	cache := newMockHintCache()
	m := &asyncModel{
		n: chainntnfs.NewTxNotifier(
			asyncStartHeight, asyncReorgLimit, cache, cache,
		),
		cache:         cache,
		currentHeight: asyncStartHeight,
		maxTip:        asyncStartHeight,
	}
	for i := range m.requests {
		r := &m.requests[i]
		r.confScript, r.confTx = staleHintTx(fmt.Sprintf("async-%d", i))
		r.outpoint, r.spendScript, r.spendTx = staleHintSpend(
			fmt.Sprintf("async-%d", i),
		)
	}

	return m
}

// sub returns the subscriber slot for ref.
func (m *asyncModel) sub(ref subRef) *asyncSubscriber {
	return &m.requests[ref.request].subs[ref.kind][ref.slot]
}

// slots returns every slot whose active state matches active.
func (m *asyncModel) slots(kind asyncKind, active bool) []subRef {
	var refs []subRef
	for i := range m.requests {
		for slot := range m.requests[i].subs[kind] {
			ref := subRef{kind: kind, request: i, slot: slot}
			if m.sub(ref).active == active {
				refs = append(refs, ref)
			}
		}
	}

	return refs
}

// RegisterConf registers a confirmation subscriber in a free slot with a
// random hint and depth.
func (m *asyncModel) RegisterConf(t *rapid.T) {
	m.registerAction(t, asyncConf)
}

// RegisterSpend registers a spend subscriber in a free slot with a random
// hint.
func (m *asyncModel) RegisterSpend(t *rapid.T) {
	m.registerAction(t, asyncSpend)
}

// registerAction draws a free slot, a hint, and a depth, then registers.
func (m *asyncModel) registerAction(t *rapid.T, kind asyncKind) {
	var free []subRef
	for _, ref := range m.slots(kind, false) {
		if !m.matured[kind][ref.request] {
			free = append(free, ref)
		}
	}
	if len(free) == 0 {
		t.Skip("no free slots")
	}

	ref := rapid.SampledFrom(free).Draw(t, "slot")
	sub := m.sub(ref)
	*sub = asyncSubscriber{
		hint: rapid.Uint32Range(
			asyncMinHint, m.currentHeight+2,
		).Draw(t, "hint"),
		numConfs: rapid.Uint32Range(1, asyncReorgLimit).Draw(
			t, "num_confs",
		),
	}
	m.register(t, ref)
}

// register registers the slot's subscriber with the notifier and queues any
// historical scan it dispatches.
func (m *asyncModel) register(t *rapid.T, ref subRef) {
	r := &m.requests[ref.request]
	sub := m.sub(ref)
	sub.active = true
	sub.delivered = false
	sub.reorgs = 0

	if ref.kind == asyncConf {
		txid := r.confTx.TxHash()
		registration, err := m.n.RegisterConf(
			&txid, r.confScript, sub.numConfs, sub.hint,
		)
		require.NoError(t, err)
		sub.confEvent = registration.Event
		if registration.HistoricalDispatch != nil {
			m.addScan(t, asyncScan{
				kind:    asyncConf,
				request: ref.request,
				conf:    registration.HistoricalDispatch,
			})
		}

		return
	}

	registration, err := m.n.RegisterSpend(
		&r.outpoint, r.spendScript, sub.hint,
	)
	require.NoError(t, err)
	sub.spendEvent = registration.Event
	if registration.HistoricalDispatch != nil {
		m.addScan(t, asyncScan{
			kind:    asyncSpend,
			request: ref.request,
			spend:   registration.HistoricalDispatch,
		})
	}
}

// addScan queues a dispatched scan after checking that its range is
// non-empty and ends at or below the tip, since a backend cannot scan blocks
// it does not have yet.
func (m *asyncModel) addScan(t *rapid.T, scan asyncScan) {
	start, end := scan.bounds()
	require.LessOrEqual(t, start, end, "empty historical scan range")
	require.LessOrEqual(t, end, m.currentHeight, "historical scan past tip")

	m.scans = append(m.scans, scan)
}

// takeScan removes and returns a random outstanding scan.
func (m *asyncModel) takeScan(t *rapid.T) asyncScan {
	if len(m.scans) == 0 {
		t.Skip("no outstanding scans")
	}

	i := rapid.IntRange(0, len(m.scans)-1).Draw(t, "scan")
	scan := m.scans[i]
	m.scans = append(m.scans[:i], m.scans[i+1:]...)

	return scan
}

// CompleteScan completes a random outstanding scan, reporting the target if
// it is mined inside the scanned range of the current chain.
func (m *asyncModel) CompleteScan(t *rapid.T) {
	scan := m.takeScan(t)
	r := &m.requests[scan.request]
	start, end := scan.bounds()
	height := r.minedHeight(scan.kind)
	found := height != 0 && start <= height && height <= end

	var err error
	if scan.kind == asyncConf {
		var details *chainntnfs.TxConfirmation
		if found {
			details = &chainntnfs.TxConfirmation{
				BlockHash:   r.confBlock.Hash(),
				BlockHeight: height,
				Tx:          r.confTx,
			}
		}
		err = m.n.UpdateConfDetails(scan.conf, details)
	} else {
		var details *chainntnfs.SpendDetail
		if found {
			spenderHash := r.spendTx.TxHash()
			details = &chainntnfs.SpendDetail{
				SpentOutPoint:  &r.outpoint,
				SpenderTxHash:  &spenderHash,
				SpendingTx:     r.spendTx,
				SpendingHeight: int32(height),
			}
		}
		err = m.n.UpdateSpendDetails(scan.spend, details)
	}

	// A set is removed when its request matures, even with a scan still
	// outstanding, in which case the late result has nowhere to go.
	if m.matured[scan.kind][scan.request] &&
		errors.Is(err, chainntnfs.ErrStaleScanResult) {

		return
	}
	require.NoError(t, err)
}

// FailScan drops a random outstanding scan without reporting a result, as a
// backend does when its rescan fails.
func (m *asyncModel) FailScan(t *rapid.T) {
	scan := m.takeScan(t)
	m.failed[scan.kind][scan.request] = true
}

// ScanProgress persists partial progress for a random outstanding spend scan,
// as Neutrino does after each block. The reported height never passes the
// spend, which the scan would have found, or the reorg window, whose blocks
// the scan may have seen before a reorg replaced them.
func (m *asyncModel) ScanProgress(t *rapid.T) {
	var spendScans []asyncScan
	for _, scan := range m.scans {
		if scan.kind == asyncSpend {
			spendScans = append(spendScans, scan)
		}
	}
	if len(spendScans) == 0 {
		t.Skip("no outstanding spend scans")
	}

	scan := rapid.SampledFrom(spendScans).Draw(t, "scan")
	start, end := scan.bounds()
	limit := min(end, m.maxTip-asyncReorgLimit)
	spendHeight := m.requests[scan.request].spendHeight
	if spendHeight != 0 && spendHeight >= start {
		limit = min(limit, spendHeight)
	}
	if limit < start {
		t.Skip("no progress to report")
	}

	height := rapid.Uint32Range(start, limit).Draw(t, "progress")
	require.NoError(t, m.cache.CommitSpendHints(
		scan.spend.ProgressHints(height),
	))
}

// Connect connects a block that is empty or carries one unmined target.
func (m *asyncModel) Connect(t *rapid.T) {
	if len(m.blocks) == asyncMaxBlocks {
		t.Skip("maximum chain length reached")
	}

	choices := []asyncBlock{{request: -1}}
	for i := range m.requests {
		if m.requests[i].confHeight == 0 {
			choices = append(choices, asyncBlock{
				kind: asyncConf, request: i,
			})
		}
		if m.requests[i].spendHeight == 0 {
			choices = append(choices, asyncBlock{
				kind: asyncSpend, request: i,
			})
		}
	}
	choice := rapid.SampledFrom(choices).Draw(t, "block")

	msgBlock := &wire.MsgBlock{}
	if choice.request >= 0 {
		r := &m.requests[choice.request]
		tx := r.confTx
		if choice.kind == asyncSpend {
			tx = r.spendTx
		}
		msgBlock.Transactions = []*wire.MsgTx{tx}
	}
	block := btcutil.NewBlock(msgBlock)
	height := m.currentHeight + 1
	require.NoError(t, m.n.ConnectTip(block, height))
	require.NoError(t, m.n.NotifyHeight(height))

	m.currentHeight = height
	m.maxTip = max(m.maxTip, height)
	m.blocks = append(m.blocks, choice)
	if choice.request < 0 {
		return
	}

	r := &m.requests[choice.request]
	if choice.kind == asyncConf {
		r.confHeight = height
		r.confBlock = block
	} else {
		r.spendHeight = height
	}
}

// Disconnect disconnects the tip, unless that would reorg deeper than the
// notifier's safety limit below the highest tip reached.
func (m *asyncModel) Disconnect(t *rapid.T) {
	if len(m.blocks) == 0 ||
		m.currentHeight+asyncReorgLimit <= m.maxTip {

		t.Skip("tip cannot be disconnected")
	}

	require.NoError(t, m.n.DisconnectTip(m.currentHeight))
	m.currentHeight--
	block := m.blocks[len(m.blocks)-1]
	m.blocks = m.blocks[:len(m.blocks)-1]
	if block.request < 0 {
		return
	}

	r := &m.requests[block.request]
	if block.kind == asyncConf {
		r.confHeight = 0
		r.confBlock = nil
	} else {
		r.spendHeight = 0
	}
	for slot := range r.subs[block.kind] {
		if r.subs[block.kind][slot].active {
			r.subs[block.kind][slot].reorgs++
		}
	}
}

// Restart tears the notifier down, drops its outstanding scans, and
// re-registers every active subscriber in a random order against a fresh
// notifier that shares the hint cache.
func (m *asyncModel) Restart(t *rapid.T) {
	m.n.TearDown()
	m.n = chainntnfs.NewTxNotifier(
		m.currentHeight, asyncReorgLimit, m.cache, m.cache,
	)
	m.scans = nil
	m.failed = [2][asyncNumRequests]bool{}

	active := append(m.slots(asyncConf, true), m.slots(asyncSpend, true)...)
	if len(active) == 0 {
		return
	}
	for _, ref := range rapid.Permutation(active).Draw(t, "order") {
		m.register(t, ref)
	}
}

// Check drains every active subscriber's notifications and asserts the
// model's invariants.
func (m *asyncModel) Check(t *rapid.T) {
	for _, kind := range []asyncKind{asyncConf, asyncSpend} {
		for _, ref := range m.slots(kind, true) {
			if kind == asyncConf {
				m.drainConf(t, ref)
			} else {
				m.drainSpend(t, ref)
			}
		}

		// Checked after draining, since a Done notification retires
		// its subscriber.
		for _, ref := range m.slots(kind, true) {
			m.checkLiveness(t, ref)
			m.checkHint(t, ref)
		}
	}
}

// depth returns the number of confirmations of a mined height.
func (m *asyncModel) depth(height uint32) uint32 {
	return m.currentHeight - height + 1
}

// drainConf consumes a confirmation subscriber's pending notifications and
// checks that each describes the chain as it is.
func (m *asyncModel) drainConf(t *rapid.T, ref subRef) {
	r := &m.requests[ref.request]
	sub := m.sub(ref)
	for {
		select {
		case details := <-sub.confEvent.Confirmed:
			require.False(
				t, sub.delivered, "duplicate confirmation",
			)
			require.NotZero(t, r.confHeight, "confirmation of "+
				"unmined transaction")
			require.Equal(t, r.confHeight, details.BlockHeight)
			require.GreaterOrEqual(
				t, m.depth(r.confHeight), sub.numConfs,
			)
			sub.delivered = true

		case <-sub.confEvent.NegativeConf:
			require.Positive(t, sub.reorgs, "negative "+
				"confirmation without a reorg")
			sub.reorgs--
			sub.delivered = false

		case <-sub.confEvent.Updates:

		case <-sub.confEvent.Done:
			require.NotZero(t, r.confHeight)
			require.Greater(
				t, m.depth(r.confHeight), asyncReorgLimit,
			)
			require.True(t, sub.delivered, "done before "+
				"confirmation")
			sub.active = false
			m.matured[asyncConf][ref.request] = true

			return

		default:
			return
		}
	}
}

// drainSpend consumes a spend subscriber's pending notifications and checks
// that each describes the chain as it is.
func (m *asyncModel) drainSpend(t *rapid.T, ref subRef) {
	r := &m.requests[ref.request]
	sub := m.sub(ref)
	for {
		select {
		case details := <-sub.spendEvent.Spend:
			require.False(t, sub.delivered, "duplicate spend")
			require.NotZero(t, r.spendHeight, "spend of unspent "+
				"outpoint")
			require.EqualValues(
				t, r.spendHeight, details.SpendingHeight,
			)
			sub.delivered = true

		case <-sub.spendEvent.Reorg:
			require.Positive(t, sub.reorgs, "spend reorg without "+
				"a reorg")
			sub.reorgs--
			sub.delivered = false

		case <-sub.spendEvent.Done:
			require.NotZero(t, r.spendHeight)
			require.Greater(
				t, m.depth(r.spendHeight), asyncReorgLimit,
			)
			require.True(t, sub.delivered, "done before spend")
			sub.active = false
			m.matured[asyncSpend][ref.request] = true

			return

		default:
			return
		}
	}
}

// owed reports whether the subscriber's hint precedes its target's current
// inclusion, in which case the notifier owes it the event.
func (m *asyncModel) owed(ref subRef) (uint32, bool) {
	height := m.requests[ref.request].minedHeight(ref.kind)

	return height, height != 0 && m.sub(ref).hint <= height
}

// checkLiveness asserts that a subscriber owed an event has received it once
// every scan for its request has reported and none failed.
func (m *asyncModel) checkLiveness(t *rapid.T, ref subRef) {
	height, owed := m.owed(ref)
	sub := m.sub(ref)
	if !owed || m.failed[ref.kind][ref.request] {
		return
	}
	if ref.kind == asyncConf && m.depth(height) < sub.numConfs {
		return
	}
	for _, scan := range m.scans {
		if scan.kind == ref.kind && scan.request == ref.request {
			return
		}
	}

	require.Truef(t, sub.delivered, "%v subscriber with hint %d missed "+
		"event at height %d", ref.kind, sub.hint, height)
}

// checkHint asserts that the cached hint would not let a subscriber owed an
// event skip it if it registered first after a restart.
func (m *asyncModel) checkHint(t *rapid.T, ref subRef) {
	height, owed := m.owed(ref)
	if !owed {
		return
	}

	r := &m.requests[ref.request]
	var (
		hint chainntnfs.HeightHint
		err  error
	)
	if ref.kind == asyncConf {
		txid := r.confTx.TxHash()
		request, reqErr := chainntnfs.NewConfRequest(
			&txid, r.confScript,
		)
		require.NoError(t, reqErr)
		hint, err = m.cache.QueryConfirmHint(request)
	} else {
		request, reqErr := chainntnfs.NewSpendRequest(
			&r.outpoint, r.spendScript,
		)
		require.NoError(t, reqErr)
		hint, err = m.cache.QuerySpendHint(request)
	}
	if err != nil {
		return
	}

	// A cached hint only applies at or above its origin.
	sub := m.sub(ref)
	start := sub.hint
	if sub.hint >= hint.Origin && hint.Height > sub.hint {
		start = hint.Height
	}
	require.LessOrEqualf(t, start, height, "%v hint %+v hides event at "+
		"height %d from subscriber with hint %d", ref.kind, hint,
		height, sub.hint)
}

// TestTxNotifierAsyncModelProperty runs asyncModel as a rapid state machine.
// It covers the orderings the earlier height hint fixes depend on: scans
// outstanding across other actions, failed scans, Neutrino progress writes,
// subscribers with differing and late hints, spends alongside confirmations,
// restarts that re-register subscribers in any order, and requests that
// mature out of the reorg window.
func TestTxNotifierAsyncModelProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		model := newAsyncModel()
		defer func() {
			model.n.TearDown()
		}()

		t.Repeat(rapid.StateMachineActions(model))
	})
}
