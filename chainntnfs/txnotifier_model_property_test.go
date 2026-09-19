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
	modelStartHeight     = uint32(100)
	modelReorgLimit      = uint32(100)
	modelNumRequests     = 3
	modelSubscriberSlots = 3
	modelMaxBlocks       = 12
)

type modelSubscriber struct {
	active          bool
	event           *chainntnfs.ConfirmationEvent
	numConfs        uint32
	lastUpdate      uint32
	delivered       bool
	expectConfirmed bool
	expectNegative  bool
	expectUpdate    bool
	expectedUpdate  chainntnfs.TxUpdateInfo
}

type modelRequest struct {
	tx          *wire.MsgTx
	txHeight    uint32
	block       *btcutil.Block
	subscribers [modelSubscriberSlots]modelSubscriber
}

type modelBlock struct {
	requestIndex int
	block        *btcutil.Block
}

type confirmationModel struct {
	n             *chainntnfs.TxNotifier
	cache         *mockHintCache
	currentHeight uint32
	requests      [modelNumRequests]modelRequest
	blocks        []modelBlock
}

type subscriberRef struct {
	requestIndex    int
	subscriberIndex int
}

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

func (m *confirmationModel) Connect(t *rapid.T) {
	if len(m.blocks) == modelMaxBlocks {
		t.Skip("maximum model chain length reached")
	}

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
			dispatch.ConfRequest, details,
		))
	}

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

func (m *confirmationModel) confirmationDepth(request *modelRequest) uint32 {
	if request.txHeight == 0 {
		return 0
	}

	return m.currentHeight - request.txHeight + 1
}

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

func TestTxNotifierModelProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		model := newConfirmationModel()
		defer func() {
			model.n.TearDown()
		}()

		t.Repeat(rapid.StateMachineActions(model))
	})
}
