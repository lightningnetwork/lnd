package lnrpc

import (
	"context"

	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
)

// MockLNConn is a mock implementation of the LightningClient interface.
// The LightningClient interface is embedded so that the methods only need to
// be implemented as needed.
type MockLNConn struct {
	mock.Mock
	LightningClient
}

func (m *MockLNConn) GetNetworkInfo(ctx context.Context, in *NetworkInfoRequest,
	opts ...grpc.CallOption) (*NetworkInfo, error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*NetworkInfo), args.Error(1)
}

func (m *MockLNConn) GetNodeInfo(ctx context.Context, in *NodeInfoRequest,
	opts ...grpc.CallOption) (*NodeInfo, error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*NodeInfo), args.Error(1)
}

func (m *MockLNConn) DescribeGraph(ctx context.Context, in *ChannelGraphRequest,
	opts ...grpc.CallOption) (*ChannelGraph, error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ChannelGraph), args.Error(1)
}

func (m *MockLNConn) GetChanInfo(ctx context.Context, in *ChanInfoRequest,
	opts ...grpc.CallOption) (*ChannelEdge, error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ChannelEdge), args.Error(1)
}
