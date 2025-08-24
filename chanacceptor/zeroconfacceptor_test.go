package chanacceptor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	testValidAddr = "bcrt1qwrmq9uca0t3dy9t9wtuq5tm4405r7tfzyqn9pp"
	testAddr, _   = chancloser.ParseUpfrontShutdownAddress(
		testValidAddr, &chaincfg.RegressionNetParams,
	)
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
	zeroAcceptor := NewZeroConfAcceptorWithOpts(
		testValidAddr, &chaincfg.RegressionNetParams,
	)

	// Assert that calling Accept won't return a failure.
	req := &ChannelAcceptRequest{
		OpenChanMsg: &lnwire.OpenChannel{},
	}
	resp := zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)

	// Add a dummyAcceptor to the zero-conf acceptor. Assert that Accept
	// does not return a failure.
	dummy := &dummyAcceptor{}
	dummyID := zeroAcceptor.AddAcceptor(dummy)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)

	// Remove the dummyAcceptor from the zero-conf acceptor and assert that
	// Accept doesn't return a failure.
	zeroAcceptor.RemoveAcceptor(dummyID)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)
}

// TestZeroConfAcceptorZC verifies that the ZeroConfAcceptor will fail
// zero-conf channel opens unless a sub-acceptor exists.
func TestZeroConfAcceptorZC(t *testing.T) {
	t.Parallel()

	// Create the zero-conf acceptor.
	zeroAcceptor := NewZeroConfAcceptorWithOpts(
		testValidAddr, &chaincfg.RegressionNetParams,
	)

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
	require.Nil(t, resp.UpfrontShutdown)

	// Add a dummyAcceptor to the zero-conf acceptor. Assert that Accept
	// does not return a failure.
	dummy := &dummyAcceptor{}
	dummyID := zeroAcceptor.AddAcceptor(dummy)
	resp = zeroAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)

	// Remove the dummyAcceptor from the zero-conf acceptor and assert that
	// Accept returns a failure.
	zeroAcceptor.RemoveAcceptor(dummyID)
	resp = zeroAcceptor.Accept(req)
	require.True(t, resp.RejectChannel())
	require.Nil(t, resp.UpfrontShutdown)
}
