//go:build chainrpc
// +build chainrpc

package chainrpc

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestRPCPkScriptNotificationIndexes ensures pkScript notification indexes and
// partial confirmation fields are preserved when converted to RPC messages.
func TestRPCPkScriptNotificationIndexes(t *testing.T) {
	var (
		fundingHash chainhash.Hash
		spendHash   chainhash.Hash
		blockHash   chainhash.Hash
	)
	fundingHash[0] = 1
	spendHash[0] = 2
	blockHash[0] = 3

	ntfn := &chainntnfs.PkScriptNotification{
		Type:       chainntnfs.PkScriptNotificationSpend,
		Height:     42,
		BlockHash:  &blockHash,
		TxHash:     &spendHash,
		TxIndex:    2,
		InputIndex: 1,
		UTXO: &chainntnfs.PkScriptUTXO{
			OutPoint: wire.OutPoint{
				Hash:  fundingHash,
				Index: 7,
			},
			Value:       btcutil.Amount(1000),
			PkScript:    []byte{0x51},
			BlockHeight: 41,
			BlockHash:   &blockHash,
			TxIndex:     3,
		},
	}

	rpcNtfn, err := rpcPkScriptNotification(ntfn)
	require.NoError(t, err)
	require.Equal(t, uint32(2), rpcNtfn.TxIndex)
	require.Equal(t, uint32(1), rpcNtfn.InputIndex)
	require.Equal(t, uint32(3), rpcNtfn.Utxo.TxIndex)
	require.Equal(t, blockHash[:], rpcNtfn.Utxo.BlockHash)

	updateNtfn := &chainntnfs.PkScriptNotification{
		Type:             chainntnfs.PkScriptNotificationConfirmUpdate,
		Height:           43,
		BlockHash:        &blockHash,
		TxHash:           &fundingHash,
		NumConfirmations: 2,
		RequiredConfs:    3,
	}

	rpcUpdate, err := rpcPkScriptNotification(updateNtfn)
	require.NoError(t, err)
	require.Equal(
		t, PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRMATION_UPDATE,
		rpcUpdate.EventType,
	)
	require.Equal(t, uint32(2), rpcUpdate.NumConfirmations)
	require.Equal(t, uint32(3), rpcUpdate.RequiredConfirmations)
}

type blockingPkScriptStream struct {
	contextFn   func() context.Context
	sendStarted chan struct{}
	releaseSend chan struct{}
}

// Send blocks until releaseSend is closed to simulate a slow RPC client.
func (b *blockingPkScriptStream) Send(*PkScriptEvent) error {
	select {
	case b.sendStarted <- struct{}{}:
	default:
	}

	<-b.releaseSend

	return nil
}

// Recv satisfies the pkScript stream interface.
func (b *blockingPkScriptStream) Recv() (*PkScriptRequest, error) {
	return nil, io.EOF
}

// SetHeader satisfies the grpc.ServerStream interface.
func (b *blockingPkScriptStream) SetHeader(metadata.MD) error {
	return nil
}

// SendHeader satisfies the grpc.ServerStream interface.
func (b *blockingPkScriptStream) SendHeader(metadata.MD) error {
	return nil
}

// SetTrailer satisfies the grpc.ServerStream interface.
func (b *blockingPkScriptStream) SetTrailer(metadata.MD) {
}

// Context returns the stream context.
func (b *blockingPkScriptStream) Context() context.Context {
	return b.contextFn()
}

// SendMsg satisfies the grpc.ServerStream interface.
func (b *blockingPkScriptStream) SendMsg(interface{}) error {
	return nil
}

// RecvMsg satisfies the grpc.ServerStream interface.
func (b *blockingPkScriptStream) RecvMsg(interface{}) error {
	return io.EOF
}

// TestPkScriptRPCEventSenderTimeout ensures slow RPC stream sends fail with a
// resource exhaustion error instead of blocking indefinitely.
func TestPkScriptRPCEventSenderTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream := &blockingPkScriptStream{
		contextFn:   func() context.Context { return ctx },
		sendStarted: make(chan struct{}, 1),
		releaseSend: make(chan struct{}),
	}
	sender := newPkScriptRPCEventSender(
		stream, make(chan struct{}), 25*time.Millisecond,
	)
	defer sender.stop()
	defer close(stream.releaseSend)

	errChan := make(chan error, 1)
	go func() {
		errChan <- sender.send(&PkScriptEvent{})
	}()

	select {
	case <-stream.sendStarted:

	case <-time.After(time.Second):
		t.Fatal("stream send did not start")
	}

	select {
	case err := <-errChan:
		require.Error(t, err)
		rpcStatus, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.ResourceExhausted, rpcStatus.Code())

	case <-time.After(time.Second):
		t.Fatal("send did not time out")
	}
}

// TestPkScriptNotificationStreamClosedErr ensures notifier-side slow-consumer
// cancellation is reported to RPC clients as resource exhaustion.
func TestPkScriptNotificationStreamClosedErr(t *testing.T) {
	t.Parallel()

	reg := &chainntnfs.PkScriptNotificationRegistration{
		Err: func() error {
			return chainntnfs.ErrPkScriptNotificationQueueFull
		},
	}

	err := pkScriptNotificationStreamClosedErr(reg)
	rpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, rpcStatus.Code())

	reg.Err = func() error {
		return chainntnfs.ErrPkScriptMatchLimit
	}
	err = pkScriptNotificationStreamClosedErr(reg)
	rpcStatus, ok = status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, rpcStatus.Code())

	reg.Err = func() error {
		return chainntnfs.ErrTxNotifierExiting
	}
	err = pkScriptNotificationStreamClosedErr(reg)
	require.ErrorIs(t, err, chainntnfs.ErrChainNotifierShuttingDown)
}
