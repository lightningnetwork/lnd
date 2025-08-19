package channeldb

import (
	"context"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	addr1 = &net.TCPAddr{IP: (net.IP)([]byte{0x1}), Port: 1}
	addr2 = &net.TCPAddr{IP: (net.IP)([]byte{0x2}), Port: 2}
	addr3 = &net.TCPAddr{IP: (net.IP)([]byte{0x3}), Port: 3}
)

// TestMultiAddrSource tests that the multiAddrSource correctly merges and
// deduplicates the results of a set of AddrSource implementations.
func TestMultiAddrSource(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var pk1 = newTestPubKey(t)

	t.Run("both sources have results", func(t *testing.T) {
		t.Parallel()

		var (
			src1 = newMockAddrSource(t)
			src2 = newMockAddrSource(t)
		)
		t.Cleanup(func() {
			src1.AssertExpectations(t)
			src2.AssertExpectations(t)
		})

		// Let source 1 know of 2 addresses (addr 1 and 2) for node 1.
		src1.On("AddrsForNode", ctx, pk1).Return(
			true, []net.Addr{addr1, addr2}, nil,
		).Once()

		// Let source 2 know of 2 addresses (addr 2 and 3) for node 1.
		src2.On("AddrsForNode", ctx, pk1).Return(
			true, []net.Addr{addr2, addr3}, nil,
			[]net.Addr{addr2, addr3}, nil,
		).Once()

		// Create a multi-addr source that consists of both source 1
		// and 2.
		multiSrc := NewMultiAddrSource(src1, src2)

		// Query it for the addresses known for node 1. The results
		// should contain addr 1, 2 and 3.
		known, addrs, err := multiSrc.AddrsForNode(ctx, pk1)
		require.NoError(t, err)
		require.True(t, known)
		require.ElementsMatch(t, addrs, []net.Addr{addr1, addr2, addr3})
	})

	t.Run("only once source has results", func(t *testing.T) {
		t.Parallel()

		var (
			src1 = newMockAddrSource(t)
			src2 = newMockAddrSource(t)
		)
		t.Cleanup(func() {
			src1.AssertExpectations(t)
			src2.AssertExpectations(t)
		})

		// Let source 1 know of address 1 for node 1.
		src1.On("AddrsForNode", ctx, pk1).Return(
			true, []net.Addr{addr1}, nil,
		).Once()
		src2.On("AddrsForNode", ctx, pk1).Return(false, nil, nil).Once()

		// Create a multi-addr source that consists of both source 1
		// and 2.
		multiSrc := NewMultiAddrSource(src1, src2)

		// Query it for the addresses known for node 1. The results
		// should contain addr 1.
		known, addrs, err := multiSrc.AddrsForNode(ctx, pk1)
		require.NoError(t, err)
		require.True(t, known)
		require.ElementsMatch(t, addrs, []net.Addr{addr1})
	})

	t.Run("unknown address", func(t *testing.T) {
		t.Parallel()

		var (
			src1 = newMockAddrSource(t)
			src2 = newMockAddrSource(t)
		)
		t.Cleanup(func() {
			src1.AssertExpectations(t)
			src2.AssertExpectations(t)
		})

		// Create a multi-addr source that consists of both source 1
		// and 2. Neither source known of node 1.
		multiSrc := NewMultiAddrSource(src1, src2)

		src1.On("AddrsForNode", ctx, pk1).Return(false, nil, nil).Once()
		src2.On("AddrsForNode", ctx, pk1).Return(false, nil, nil).Once()

		// Query it for the addresses known for node 1. It should return
		// false to indicate that the node is unknown to all backing
		// sources.
		known, addrs, err := multiSrc.AddrsForNode(ctx, pk1)
		require.NoError(t, err)
		require.False(t, known)
		require.Empty(t, addrs)
	})
}

type mockAddrSource struct {
	t *testing.T
	mock.Mock
}

var _ AddrSource = (*mockAddrSource)(nil)

func newMockAddrSource(t *testing.T) *mockAddrSource {
	return &mockAddrSource{t: t}
}

func (m *mockAddrSource) AddrsForNode(ctx context.Context,
	pub *btcec.PublicKey) (bool, []net.Addr, error) {

	args := m.Called(ctx, pub)
	if args.Get(1) == nil {
		return args.Bool(0), nil, args.Error(2)
	}

	addrs, ok := args.Get(1).([]net.Addr)
	require.True(m.t, ok)

	return args.Bool(0), addrs, args.Error(2)
}

func newTestPubKey(t *testing.T) *btcec.PublicKey {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}
