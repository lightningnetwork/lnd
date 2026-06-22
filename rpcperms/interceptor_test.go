package rpcperms

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPanicRecoveryUnaryServerInterceptor asserts that unary handler panics are
// converted to internal RPC errors rather than propagating to the process.
func TestPanicRecoveryUnaryServerInterceptor(t *testing.T) {
	interceptor := panicRecoveryUnaryServerInterceptor(btclog.Disabled)
	info := &grpc.UnaryServerInfo{
		FullMethod: "/test.Service/Unary",
	}

	resp, err := interceptor(
		t.Context(), nil, info,
		func(context.Context, any) (any, error) {
			panic("boom")
		},
	)
	require.Nil(t, resp)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))

	expectedResp := struct{}{}
	expectedErr := errors.New("handler error")
	resp, err = interceptor(
		t.Context(), nil, info,
		func(context.Context, any) (any, error) {
			return expectedResp, expectedErr
		},
	)
	require.Equal(t, expectedResp, resp)
	require.ErrorIs(t, err, expectedErr)

	var nilLogger btclog.Logger
	interceptor = panicRecoveryUnaryServerInterceptor(nilLogger)
	resp, err = interceptor(
		t.Context(), nil, info,
		func(context.Context, any) (any, error) {
			panic("boom")
		},
	)
	require.Nil(t, resp)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

// TestPanicRecoveryStreamServerInterceptor asserts that stream handler panics
// are converted to internal RPC errors rather than propagating to the process.
func TestPanicRecoveryStreamServerInterceptor(t *testing.T) {
	interceptor := panicRecoveryStreamServerInterceptor(btclog.Disabled)
	info := &grpc.StreamServerInfo{
		FullMethod: "/test.Service/Stream",
	}

	err := interceptor(
		nil, nil, info, func(any, grpc.ServerStream) error {
			panic("boom")
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))

	expectedErr := errors.New("handler error")
	err = interceptor(
		nil, nil, info, func(any, grpc.ServerStream) error {
			return expectedErr
		},
	)
	require.ErrorIs(t, err, expectedErr)

	var nilLogger btclog.Logger
	interceptor = panicRecoveryStreamServerInterceptor(nilLogger)
	err = interceptor(
		nil, nil, info, func(any, grpc.ServerStream) error {
			panic("boom")
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))

	var stream recordingServerStream
	err = interceptor(
		nil, &stream, info, func(_ any, ss grpc.ServerStream) error {
			require.NoError(t, ss.SendMsg(struct{}{}))
			panic("boom")
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
	require.Equal(t, 1, stream.numSent)
}

type recordingServerStream struct {
	grpc.ServerStream
	numSent int
}

func (s *recordingServerStream) SendMsg(any) error {
	s.numSent++
	return nil
}

// TestTruncatePanicStack asserts that panic stack traces are capped with a
// readable truncation marker.
func TestTruncatePanicStack(t *testing.T) {
	shortStack := []byte("short stack")
	require.Equal(t, shortStack, truncatePanicStack(shortStack))

	longStack := bytes.Repeat([]byte("stack frame\n"), maxPanicStackSize)
	truncatedStack := truncatePanicStack(longStack)

	require.LessOrEqual(t, len(truncatedStack), maxPanicStackSize)
	require.True(
		t, bytes.HasSuffix(
			truncatedStack, []byte(panicStackTruncatedMsg),
		),
	)
}
