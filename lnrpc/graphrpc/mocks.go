package graphrpc

import (
	"context"

	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
)

// MockGraphClient is a mock implementation of the GraphClient interface.
type MockGraphClient struct {
	mock.Mock
}

// A compile-time interface to ensure that MockGraphClient satisfies the
// GraphClient interface.
var _ GraphClient = (*MockGraphClient)(nil)

func (m *MockGraphClient) BootstrapperName(ctx context.Context,
	in *BoostrapperNameReq, opts ...grpc.CallOption) (*BoostrapperNameResp,
	error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*BoostrapperNameResp), args.Error(1)
}

func (m *MockGraphClient) BootstrapAddrs(ctx context.Context,
	in *BootstrapAddrsReq, opts ...grpc.CallOption) (*BootstrapAddrsResp,
	error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*BootstrapAddrsResp), args.Error(1)
}

func (m *MockGraphClient) BetweennessCentrality(ctx context.Context,
	in *BetweennessCentralityReq, opts ...grpc.CallOption) (
	*BetweennessCentralityResp, error) {

	args := m.Called(ctx, in, opts)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*BetweennessCentralityResp), args.Error(1)
}
