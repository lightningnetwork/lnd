//go:build chainrpc
// +build chainrpc

package chainrpc

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type mockConfStream struct {
	grpc.ServerStream

	context func() context.Context
	events  []*ConfEvent
}

func (m *mockConfStream) Context() context.Context {
	return m.context()
}

func (m *mockConfStream) Send(event *ConfEvent) error {
	m.events = append(m.events, event)

	return nil
}

type mockSpendStream struct {
	grpc.ServerStream

	context func() context.Context
	events  []*SpendEvent
}

func (m *mockSpendStream) Context() context.Context {
	return m.context()
}

func (m *mockSpendStream) Send(event *SpendEvent) error {
	m.events = append(m.events, event)

	return nil
}

// TestConfFinalEventOrder verifies that a historical confirmation which is
// already mature is delivered before its terminal event, even if the RPC
// handler observes the Done channel first.
func TestConfFinalEventOrder(t *testing.T) {
	t.Parallel()

	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On("Started").Return(true).Once()

	confEvent := chainntnfs.NewConfirmationEvent(1, func() {})
	confEvent.Confirmed = make(chan *chainntnfs.TxConfirmation)
	confEvent.Done <- struct{}{}

	blockHash := chainhash.Hash{}
	go func() {
		select {
		case confEvent.Confirmed <- &chainntnfs.TxConfirmation{
			BlockHash: &blockHash,
			Tx:        wire.NewMsgTx(2),
		}:
		case <-t.Context().Done():
		}
	}()

	notifier.On(
		"RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, uint32(1), uint32(1),
	).Return(confEvent, nil).Once()

	server := &Server{
		cfg: Config{
			ChainNotifier: notifier,
		},
		quit: make(chan struct{}),
	}
	stream := &mockConfStream{
		context: t.Context,
	}

	err := server.RegisterConfirmationsNtfn(&ConfRequest{
		Script:     []byte{0x01},
		NumConfs:   1,
		HeightHint: 1,
	}, stream)
	require.NoError(t, err)
	require.Len(t, stream.events, 2)
	require.IsType(t, &ConfEvent_Conf{}, stream.events[0].Event)
	require.IsType(t, &ConfEvent_Done{}, stream.events[1].Event)
	notifier.AssertExpectations(t)
}

// TestSpendFinalEventOrder verifies that a historical spend which is already
// mature is delivered before its terminal event, even if the RPC handler
// observes the Done channel first.
func TestSpendFinalEventOrder(t *testing.T) {
	t.Parallel()

	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On("Started").Return(true).Once()

	spendEvent := chainntnfs.NewSpendEvent(func() {})
	spendEvent.Spend = make(chan *chainntnfs.SpendDetail)
	spendEvent.Done <- struct{}{}

	outpoint := wire.OutPoint{}
	spenderHash := chainhash.Hash{}
	go func() {
		select {
		case spendEvent.Spend <- &chainntnfs.SpendDetail{
			SpentOutPoint: &outpoint,
			SpenderTxHash: &spenderHash,
			SpendingTx:    wire.NewMsgTx(2),
		}:
		case <-t.Context().Done():
		}
	}()

	notifier.On(
		"RegisterSpendNtfn", mock.Anything, mock.Anything,
		uint32(1),
	).Return(spendEvent, nil).Once()

	server := &Server{
		cfg: Config{
			ChainNotifier: notifier,
		},
		quit: make(chan struct{}),
	}
	stream := &mockSpendStream{
		context: t.Context,
	}

	err := server.RegisterSpendNtfn(&SpendRequest{
		Script:     []byte{0x01},
		HeightHint: 1,
	}, stream)
	require.NoError(t, err)
	require.Len(t, stream.events, 2)
	require.IsType(t, &SpendEvent_Spend{}, stream.events[0].Event)
	require.IsType(t, &SpendEvent_Done{}, stream.events[1].Event)
	notifier.AssertExpectations(t)
}

// TestConfReorgLifecycle verifies that the RPC preserves the complete
// positive-reorg-positive-Done lifecycle and forwards the re-org depth.
func TestConfReorgLifecycle(t *testing.T) {
	t.Parallel()

	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On("Started").Return(true).Once()

	confEvent := chainntnfs.NewConfirmationEvent(1, func() {})
	confEvent.Confirmed = make(chan *chainntnfs.TxConfirmation)
	confEvent.NegativeConf = make(chan int32)
	confEvent.Done = make(chan struct{})

	blockHash := chainhash.Hash{}
	newConfirmation := func() *chainntnfs.TxConfirmation {
		return &chainntnfs.TxConfirmation{
			BlockHash: &blockHash,
			Tx:        wire.NewMsgTx(2),
		}
	}
	go func() {
		confEvent.Confirmed <- newConfirmation()
		confEvent.NegativeConf <- 7
		confEvent.Confirmed <- newConfirmation()
		confEvent.Done <- struct{}{}
	}()

	notifier.On(
		"RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, uint32(1), uint32(1),
	).Return(confEvent, nil).Once()

	server := &Server{
		cfg: Config{
			ChainNotifier: notifier,
		},
		quit: make(chan struct{}),
	}
	stream := &mockConfStream{
		context: t.Context,
	}

	err := server.RegisterConfirmationsNtfn(&ConfRequest{
		Script:     []byte{0x01},
		NumConfs:   1,
		HeightHint: 1,
	}, stream)
	require.NoError(t, err)
	require.Len(t, stream.events, 4)
	require.IsType(t, &ConfEvent_Conf{}, stream.events[0].Event)
	require.EqualValues(t, 7, stream.events[1].GetReorg().Depth)
	require.IsType(t, &ConfEvent_Conf{}, stream.events[2].Event)
	require.IsType(t, &ConfEvent_Done{}, stream.events[3].Event)
	notifier.AssertExpectations(t)
}

// TestSpendReorgLifecycle verifies that the RPC preserves the complete
// spend-reorg-spend-Done lifecycle.
func TestSpendReorgLifecycle(t *testing.T) {
	t.Parallel()

	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On("Started").Return(true).Once()

	spendEvent := chainntnfs.NewSpendEvent(func() {})
	spendEvent.Spend = make(chan *chainntnfs.SpendDetail)
	spendEvent.Reorg = make(chan struct{})
	spendEvent.Done = make(chan struct{})

	outpoint := wire.OutPoint{}
	spenderHash := chainhash.Hash{}
	newSpend := func() *chainntnfs.SpendDetail {
		return &chainntnfs.SpendDetail{
			SpentOutPoint: &outpoint,
			SpenderTxHash: &spenderHash,
			SpendingTx:    wire.NewMsgTx(2),
		}
	}
	go func() {
		spendEvent.Spend <- newSpend()
		spendEvent.Reorg <- struct{}{}
		spendEvent.Spend <- newSpend()
		spendEvent.Done <- struct{}{}
	}()

	notifier.On(
		"RegisterSpendNtfn", mock.Anything, mock.Anything,
		uint32(1),
	).Return(spendEvent, nil).Once()

	server := &Server{
		cfg: Config{
			ChainNotifier: notifier,
		},
		quit: make(chan struct{}),
	}
	stream := &mockSpendStream{
		context: t.Context,
	}

	err := server.RegisterSpendNtfn(&SpendRequest{
		Script:     []byte{0x01},
		HeightHint: 1,
	}, stream)
	require.NoError(t, err)
	require.Len(t, stream.events, 4)
	require.IsType(t, &SpendEvent_Spend{}, stream.events[0].Event)
	require.IsType(t, &SpendEvent_Reorg{}, stream.events[1].Event)
	require.IsType(t, &SpendEvent_Spend{}, stream.events[2].Event)
	require.IsType(t, &SpendEvent_Done{}, stream.events[3].Event)
	notifier.AssertExpectations(t)
}
