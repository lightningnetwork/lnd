package chainntnfs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const (
	// modelStartHeight is the TxNotifier's starting chain height for
	// every run of the state machine.
	modelStartHeight = uint32(100)

	// modelReorgLimit is the reorg safety limit passed to the
	// TxNotifier, bounding how far back a request's history is retained.
	modelReorgLimit = uint32(100)

	// modelNumRequests is the number of distinct transactions the model
	// tracks. Each has its own script and can be mined independently of
	// the others.
	modelNumRequests = 3

	// modelSubscriberSlots is the number of concurrent subscriber slots
	// available per request. Each slot can be registered, canceled, and
	// re-registered independently.
	modelSubscriberSlots = 3

	// modelMaxBlocks bounds the length of the model's simulated chain,
	// keeping each rapid.Check run finite.
	modelMaxBlocks = 12
)

// modelSubscriber tracks one subscriber slot for one request, both its
// TxNotifier-facing state and the oracle's expectation of what that
// subscriber should observe on its next Check call.
type modelSubscriber struct {
	// active is true if this slot currently holds a live registration.
	active bool

	// event is the ConfirmationEvent returned by the active
	// registration.
	event *chainntnfs.ConfirmationEvent

	// numConfs is the confirmation depth this subscriber registered
	// with.
	numConfs uint32

	// lastUpdate is the numConfsLeft value of the most recent update the
	// oracle expects to have been sent to this subscriber, used to avoid
	// re-expecting the same update.
	lastUpdate uint32

	// delivered is true once the oracle has accounted for the
	// confirmation notification for the request's current inclusion, so
	// that reconnecting the same block does not re-trigger it.
	delivered bool

	// expectConfirmed is true if the oracle expects a confirmation
	// notification to be waiting the next time Check runs.
	expectConfirmed bool

	// expectNegative is true if the oracle expects a negative
	// confirmation (reorg) notification to be waiting the next time
	// Check runs.
	expectNegative bool

	// expectUpdate is true if the oracle expects a confirmation update to
	// be waiting the next time Check runs.
	expectUpdate bool

	// expectedUpdate is the update the oracle expects, valid only when
	// expectUpdate is true.
	expectedUpdate chainntnfs.TxUpdateInfo
}

// modelRequest is one of the model's fixed transactions, along with the
// height at which it is currently mined (zero if it is not currently part
// of the model's chain) and the subscriber slots registered against it.
type modelRequest struct {
	// tx is this request's transaction.
	tx *wire.MsgTx

	// txHeight is the height at which tx is currently mined, or zero if
	// it is not part of the model's chain right now.
	txHeight uint32

	// block is the block containing tx, valid only when txHeight is
	// nonzero.
	block *btcutil.Block

	// subscribers holds the subscriber slots registered against this
	// request.
	subscribers [modelSubscriberSlots]modelSubscriber
}

// modelBlock records one block connected to the model's chain, and which
// request, if any, it carried. It lets Disconnect undo exactly what Connect
// did.
type modelBlock struct {
	// requestIndex is the index into confirmationModel.requests of the
	// transaction this block carries, or -1 if the block is empty.
	requestIndex int

	// block is the connected block itself.
	block *btcutil.Block
}

// confirmationModel is the state machine model driven by rapid's
// StateMachineActions. It wraps a TxNotifier under test alongside an oracle
// that independently predicts what each active subscriber should observe,
// so that Check can compare the two after every action. See
// TestTxNotifierModelProperty for how the actions below are assembled into
// a run.
type confirmationModel struct {
	// n is the TxNotifier under test.
	n *chainntnfs.TxNotifier

	// cache is the height hint cache backing n, shared across restarts
	// within a run.
	cache *mockHintCache

	// currentHeight is the model's simulated chain tip.
	currentHeight uint32

	// requests holds the model's fixed set of transactions and their
	// subscriber slots.
	requests [modelNumRequests]modelRequest

	// blocks is the model's simulated chain, most recently connected
	// block last.
	blocks []modelBlock
}

// subscriberRef identifies a single subscriber slot by its request and
// slot index.
type subscriberRef struct {
	requestIndex    int
	subscriberIndex int
}

// newConfirmationModel constructs a confirmationModel with a fresh
// TxNotifier and hint cache, and a fixed set of modelNumRequests
// transactions, none of which are yet mined or subscribed to.
func newConfirmationModel() *confirmationModel {
	cache := newMockHintCache()
	m := &confirmationModel{
		n: chainntnfs.NewTxNotifier(
			modelStartHeight, modelReorgLimit, cache, cache,
		),
		cache:         cache,
		currentHeight: modelStartHeight,
	}

	for i := range m.requests {
		program := sha256.Sum256([]byte{byte(i + 1)})
		script := append([]byte{0x51, 0x20}, program[:]...)
		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(i + 1),
			PkScript: script,
		})
		m.requests[i].tx = tx
	}

	return m
}

// Register is a rapid state-machine action that picks a random inactive
// subscriber slot and a random confirmation depth, then registers it. It is
// a no-op action (via t.Skip) when every slot is already active, which
// keeps the model's fixed subscriber-slot layout from ever overflowing.
func (m *confirmationModel) Register(t *rapid.T) {
	var available []subscriberRef
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		for subscriberIndex := range request.subscribers {
			subscriber := &request.subscribers[subscriberIndex]
			if !subscriber.active {
				available = append(available, subscriberRef{
					requestIndex:    requestIndex,
					subscriberIndex: subscriberIndex,
				})
			}
		}
	}
	if len(available) == 0 {
		t.Skip("all subscriber slots are active")
	}

	ref := rapid.SampledFrom(available).Draw(t, "subscriber")
	numConfs := rapid.Uint32Range(1, 5).Draw(t, "num_confs")
	m.register(t, ref, numConfs)
}

// Cancel is a rapid state-machine action that picks a random active
// subscriber and cancels it, then asserts that its Confirmed channel is
// closed, which is how the TxNotifier signals a canceled subscription.
func (m *confirmationModel) Cancel(t *rapid.T) {
	active := m.activeSubscribers()
	if len(active) == 0 {
		t.Skip("no active subscribers")
	}

	ref := rapid.SampledFrom(active).Draw(t, "subscriber")
	request := &m.requests[ref.requestIndex]
	subscriber := &request.subscribers[ref.subscriberIndex]
	subscriber.event.Cancel()
	subscriber.active = false

	select {
	case _, ok := <-subscriber.event.Confirmed:
		require.False(t, ok)
	default:
		t.Fatal("canceled confirmation channel is open")
	}
}

// Connect is a rapid state-machine action that connects one new block to the
// model's chain. The block is either empty or carries one of the requests
// not currently mined, chosen at random. After connecting, it updates the
// oracle's expectations for every active subscriber via markUpdates and
// markConfirmations.
func (m *confirmationModel) Connect(t *rapid.T) {
	if len(m.blocks) == modelMaxBlocks {
		t.Skip("maximum model chain length reached")
	}

	// Choose which transaction, if any, this block carries. -1 means an
	// empty block; only requests not already mined are eligible.
	available := []int{-1}
	for i := range m.requests {
		if m.requests[i].txHeight == 0 {
			available = append(available, i)
		}
	}
	requestIndex := rapid.SampledFrom(available).Draw(t, "request")

	msgBlock := &wire.MsgBlock{}
	if requestIndex >= 0 {
		msgBlock.Transactions = []*wire.MsgTx{
			m.requests[requestIndex].tx,
		}
	}
	block := btcutil.NewBlock(msgBlock)
	height := m.currentHeight + 1
	require.NoError(t, m.n.ConnectTip(block, height))
	require.NoError(t, m.n.NotifyHeight(height))

	// Record the block in the model's chain and, if it carried a
	// request, mark that request as mined at this height.
	m.currentHeight = height
	m.blocks = append(m.blocks, modelBlock{
		requestIndex: requestIndex,
		block:        block,
	})
	if requestIndex >= 0 {
		request := &m.requests[requestIndex]
		request.txHeight = height
		request.block = block
	}
	m.markUpdates()
	m.markConfirmations()
}

// Disconnect is a rapid state-machine action that disconnects the most
// recently connected block. If that block carried a request, the request is
// marked unmined again and every active subscriber on it is expected to
// receive a negative confirmation. Every other active subscriber on a still-
// mined request has its lastUpdate reset to its full confirmation depth, so
// markUpdates will correctly re-expect updates as the chain is rebuilt.
func (m *confirmationModel) Disconnect(t *rapid.T) {
	if len(m.blocks) == 0 {
		t.Skip("model chain is empty")
	}

	lastIndex := len(m.blocks) - 1
	block := m.blocks[lastIndex]
	require.NoError(t, m.n.DisconnectTip(m.currentHeight))
	m.blocks = m.blocks[:lastIndex]
	m.currentHeight--
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		if request.txHeight == 0 {
			continue
		}

		for i := range request.subscribers {
			subscriber := &request.subscribers[i]
			if subscriber.active {
				subscriber.lastUpdate = subscriber.numConfs
			}
		}
	}

	if block.requestIndex < 0 {
		return
	}

	request := &m.requests[block.requestIndex]
	request.txHeight = 0
	request.block = nil
	for i := range request.subscribers {
		subscriber := &request.subscribers[i]
		if !subscriber.active {
			continue
		}

		subscriber.delivered = false
		subscriber.expectNegative = true
	}
}

// Restart is a rapid state-machine action that tears the notifier down and
// rebuilds it at the same chain height, sharing the same hint cache. Every
// active subscriber must observe its Confirmed channel closed by the
// teardown, and is then re-registered against the fresh notifier so the
// scenario can continue as if it had reconnected after a real process
// restart.
func (m *confirmationModel) Restart(t *rapid.T) {
	active := m.activeSubscribers()
	m.n.TearDown()
	for _, ref := range active {
		request := &m.requests[ref.requestIndex]
		subscriber := &request.subscribers[ref.subscriberIndex]
		select {
		case _, ok := <-subscriber.event.Confirmed:
			require.False(t, ok)
		default:
			t.Fatal("shutdown confirmation channel is open")
		}
		subscriber.delivered = false
	}

	m.n = chainntnfs.NewTxNotifier(
		m.currentHeight, modelReorgLimit, m.cache, m.cache,
	)
	for _, ref := range active {
		request := &m.requests[ref.requestIndex]
		subscriber := &request.subscribers[ref.subscriberIndex]
		m.register(t, ref, subscriber.numConfs)
	}
}

// Check is a rapid state-machine action, run after every other action, that
// compares the oracle's expectations against every active subscriber's
// actual TxNotifier channels. It is the assertion point for the whole
// model: any divergence between what markConfirmations, markUpdates, and
// register expected and what the TxNotifier actually delivered fails the
// test here. It also asserts that no subscriber's Done channel has fired,
// since none of the model's requests are meant to mature (fall out of the
// reorg safety window) within the bounded chain length used by this model.
func (m *confirmationModel) Check(t *rapid.T) {
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		for subscriberIndex := range request.subscribers {
			subscriber := &request.subscribers[subscriberIndex]
			if !subscriber.active {
				continue
			}

			m.checkConfirmed(t, request, subscriber)
			m.checkNegative(t, subscriber)
			m.checkUpdates(t, request, subscriber)

			select {
			case <-subscriber.event.Done:
				t.Fatal("request matured inside model window")
			default:
			}
		}
	}
}

// register performs the actual TxNotifier registration for ref with the
// given confirmation depth, resets that slot's modelSubscriber state, and
// updates the oracle. If the registration triggers a historical dispatch,
// this completes it immediately, delivering the request's current
// confirmation details if the request's mined height falls within the
// dispatch's scanned range. It then seeds the oracle's expectations: an
// immediate confirmation if the request already meets this subscriber's
// depth, and a confirmation update for this subscriber and for every other
// subscriber on the same request that has not yet had its confirmation
// delivered, since a shared historical dispatch can change what all of them
// are due.
func (m *confirmationModel) register(t *rapid.T, ref subscriberRef,
	numConfs uint32) {

	request := &m.requests[ref.requestIndex]
	txid := request.tx.TxHash()
	script := request.tx.TxOut[0].PkScript
	registration, err := m.n.RegisterConf(
		&txid, script, numConfs, modelStartHeight,
	)
	require.NoError(t, err)

	subscriber := &request.subscribers[ref.subscriberIndex]
	*subscriber = modelSubscriber{
		active:     true,
		event:      registration.Event,
		numConfs:   numConfs,
		lastUpdate: numConfs,
	}

	// If a historical scan was dispatched, complete it now, reporting a
	// confirmation only if the request's current mined height falls
	// inside the scanned range.
	if registration.HistoricalDispatch != nil {
		var details *chainntnfs.TxConfirmation
		dispatch := registration.HistoricalDispatch
		if request.txHeight >= dispatch.StartHeight &&
			request.txHeight <= dispatch.EndHeight {

			details = &chainntnfs.TxConfirmation{
				BlockHash:   request.block.Hash(),
				BlockHeight: request.txHeight,
				Tx:          request.tx,
			}
		}
		require.NoError(t, m.n.UpdateConfDetails(
			dispatch, details,
		))
	}

	// Seed the oracle: this subscriber may already meet its depth, and
	// every subscriber sharing this request that has not yet had its
	// confirmation delivered is due a fresh update now that the request
	// set's scan state has changed.
	if m.confirmationDepth(request) >= subscriber.numConfs {
		subscriber.delivered = true
		subscriber.expectConfirmed = true
	}
	for i := range request.subscribers {
		active := &request.subscribers[i]
		shouldUpdate := active == subscriber || !active.delivered
		if active.active && shouldUpdate {
			m.expectUpdate(request, active, true)
		}
	}
}

// activeSubscribers returns a subscriberRef for every currently active
// subscriber slot, across all requests.
func (m *confirmationModel) activeSubscribers() []subscriberRef {
	var active []subscriberRef
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		for subscriberIndex := range request.subscribers {
			if request.subscribers[subscriberIndex].active {
				active = append(active, subscriberRef{
					requestIndex:    requestIndex,
					subscriberIndex: subscriberIndex,
				})
			}
		}
	}

	return active
}

// markConfirmations updates the oracle's expectConfirmed flag for every
// active subscriber whose request has reached its required confirmation
// depth and has not already had a confirmation delivered. It is called
// after a block connects, since that is the only action that can push a
// request's confirmation depth up to a subscriber's threshold.
func (m *confirmationModel) markConfirmations() {
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		depth := m.confirmationDepth(request)
		for i := range request.subscribers {
			subscriber := &request.subscribers[i]
			if !subscriber.active || subscriber.delivered ||
				depth < subscriber.numConfs {

				continue
			}

			subscriber.delivered = true
			subscriber.expectConfirmed = true
		}
	}
}

// markUpdates calls expectUpdate for every active subscriber on every
// currently mined request, in non-registration mode. It is called after a
// block connects, since a new tip height can change how many confirmations
// are left for a subscriber that has not yet been fully confirmed.
func (m *confirmationModel) markUpdates() {
	for requestIndex := range m.requests {
		request := &m.requests[requestIndex]
		if request.txHeight == 0 {
			continue
		}

		for i := range request.subscribers {
			subscriber := &request.subscribers[i]
			if subscriber.active {
				m.expectUpdate(request, subscriber, false)
			}
		}
	}
}

// expectUpdate computes whether subscriber is due a confirmation update for
// request at the model's current height, and if so, records it as the
// oracle's next expected update. registration distinguishes the two
// callers: markUpdates (false) skips a request whose confirmation height
// has already passed, since a live block connection would not resend an
// update for a depth already met, while register (true) still expects one,
// since a fresh registration always reports the current number of
// confirmations left. lastUpdate deduplicates repeated calls for the same
// subscriber by only accepting a strictly smaller numConfsLeft than the one
// already expected or delivered.
func (m *confirmationModel) expectUpdate(request *modelRequest,
	subscriber *modelSubscriber, registration bool) {

	if request.txHeight == 0 {
		return
	}

	confirmationHeight := request.txHeight + subscriber.numConfs - 1
	if confirmationHeight < m.currentHeight && !registration {
		return
	}

	var numConfsLeft uint32
	if confirmationHeight > m.currentHeight {
		numConfsLeft = confirmationHeight - m.currentHeight
	}
	if numConfsLeft >= subscriber.lastUpdate {
		return
	}

	subscriber.lastUpdate = numConfsLeft
	subscriber.expectUpdate = true
	subscriber.expectedUpdate = chainntnfs.TxUpdateInfo{
		NumConfsLeft: numConfsLeft,
		BlockHeight:  request.txHeight,
	}
}

// confirmationDepth returns the number of confirmations request currently
// has at the model's current height, or zero if the request is not mined.
func (m *confirmationModel) confirmationDepth(request *modelRequest) uint32 {
	if request.txHeight == 0 {
		return 0
	}

	return m.currentHeight - request.txHeight + 1
}

// checkConfirmed compares subscriber's Confirmed channel against the
// oracle's expectConfirmed flag, failing the test on a mismatch in either
// direction, and also fails if a second confirmation is waiting, since each
// request is expected to deliver at most one per inclusion.
func (m *confirmationModel) checkConfirmed(t *rapid.T,
	request *modelRequest, subscriber *modelSubscriber) {

	select {
	case details := <-subscriber.event.Confirmed:
		if !subscriber.expectConfirmed {
			t.Fatal("unexpected confirmation")
		}
		require.Equal(t, request.txHeight, details.BlockHeight)
		require.Equal(t, request.tx.TxHash(), details.Tx.TxHash())
		subscriber.expectConfirmed = false
	default:
		if subscriber.expectConfirmed {
			t.Fatal("missing confirmation")
		}
	}

	select {
	case <-subscriber.event.Confirmed:
		t.Fatal("duplicate confirmation")
	default:
	}
}

// checkNegative compares subscriber's NegativeConf channel against the
// oracle's expectNegative flag, failing the test on a mismatch in either
// direction, and also fails if a second negative confirmation is waiting.
func (m *confirmationModel) checkNegative(t *rapid.T,
	subscriber *modelSubscriber) {

	select {
	case depth := <-subscriber.event.NegativeConf:
		if !subscriber.expectNegative {
			t.Fatal("unexpected negative confirmation")
		}
		require.Greater(t, depth, int32(0))
		subscriber.expectNegative = false
	default:
		if subscriber.expectNegative {
			t.Fatal("missing negative confirmation")
		}
	}

	select {
	case <-subscriber.event.NegativeConf:
		t.Fatal("duplicate negative confirmation")
	default:
	}
}

// checkUpdates compares subscriber's Updates channel against the oracle's
// expectUpdate flag and expectedUpdate value, failing the test on a
// mismatch in either direction, and also fails if a second update is
// waiting.
func (m *confirmationModel) checkUpdates(t *rapid.T,
	_ *modelRequest, subscriber *modelSubscriber) {

	select {
	case update := <-subscriber.event.Updates:
		if !subscriber.expectUpdate {
			t.Fatal("unexpected confirmation update")
		}
		require.Equal(t, subscriber.expectedUpdate, update)
		subscriber.expectUpdate = false
	default:
		if subscriber.expectUpdate {
			t.Fatal("missing confirmation update")
		}
	}

	select {
	case <-subscriber.event.Updates:
		t.Fatal("duplicate confirmation update")
	default:
	}
}

// TestTxNotifierModelProperty runs confirmationModel as a rapid state
// machine, repeatedly picking at random from its Register, Cancel, Connect,
// Disconnect, and Restart actions, and checking the model's oracle against
// the real TxNotifier via Check after each one. Unlike the fixed scenarios
// in confirmation_registration_order_test.go and the targeted regressions
// in txnotifier_stale_hint_test.go, this test explores arbitrarily long,
// arbitrarily interleaved sequences of registration, cancellation, chain
// reorganization, and restart across multiple subscribers and multiple
// independent requests at once. It is the broadest check that the
// notifier's confirmation, update, and negative-confirmation contract holds
// under any such sequence, not just the specific orderings the other tests
// construct by hand.
func TestTxNotifierModelProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		model := newConfirmationModel()
		defer func() {
			model.n.TearDown()
		}()

		t.Repeat(rapid.StateMachineActions(model))
	})
}
