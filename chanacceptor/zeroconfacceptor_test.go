package chanacceptor

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// dummyAcceptor is a ChannelAcceptor that will never return a failure.
type dummyAcceptor struct{}

func (d *dummyAcceptor) Accept(
	req *ChannelAcceptRequest) *ChannelAcceptResponse {

	return &ChannelAcceptResponse{}
}

// TestZeroConfAcceptorNormal verifies that the ZeroConfAcceptor will let
// requests go through for non-zero-conf channels if there are no
// sub-acceptors.
func TestZeroConfAcceptorNormal(t *testing.T) {
	t.Parallel()

	// Create the zero-conf acceptor.
	zeroAcceptor := NewZeroConfAcceptor()

	// Assert that calling Accept won't return a failure.
	req := &ChannelAcceptRequest{
		OpenChanMsg: &lnwire.OpenChannel{},
	}
	resp := zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())

	// Add a dummyAcceptor to the zero-conf acceptor. Assert that Accept
	// does not return a failure.
	dummy := &dummyAcceptor{}
	dummyID := zeroAcceptor.AddAcceptor(dummy)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())

	// Remove the dummyAcceptor from the zero-conf acceptor and assert that
	// Accept doesn't return a failure.
	zeroAcceptor.RemoveAcceptor(dummyID)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
}

// TestZeroConfAcceptorZC verifies that the ZeroConfAcceptor will fail
// zero-conf channel opens unless a sub-acceptor exists.
func TestZeroConfAcceptorZC(t *testing.T) {
	t.Parallel()

	// Create the zero-conf acceptor.
	zeroAcceptor := NewZeroConfAcceptor()

	channelType := new(lnwire.ChannelType)
	*channelType = lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.ZeroConfRequired,
	))

	// Assert that calling Accept results in failure.
	req := &ChannelAcceptRequest{
		OpenChanMsg: &lnwire.OpenChannel{
			ChannelType: channelType,
		},
	}
	resp := zeroAcceptor.Accept(req)
	require.True(t, resp.RejectChannel())

	// Add a dummyAcceptor to the zero-conf acceptor. Assert that Accept
	// does not return a failure.
	dummy := &dummyAcceptor{}
	dummyID := zeroAcceptor.AddAcceptor(dummy)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())

	// Remove the dummyAcceptor from the zero-conf acceptor and assert that
	// Accept returns a failure.
	zeroAcceptor.RemoveAcceptor(dummyID)
	resp = zeroAcceptor.Accept(req)
	require.True(t, resp.RejectChannel())
}
