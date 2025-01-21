package chanacceptor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestChainedAcceptorNoOpts verifies that the ChainedAcceptor will
// return nil UpfrontShutdown when not specified.
func TestChainedAcceptorNoOpts(t *testing.T) {
	t.Parallel()

	// Create the chained acceptor.
	chainedAcceptor := NewChainedAcceptor()

	// Assert that calling Accept return nil UpfrontShutdown.
	req := &ChannelAcceptRequest{
		OpenChanMsg: &lnwire.OpenChannel{},
	}
	resp := chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Nil(t, resp.UpfrontShutdown)

	// Add a dummyAcceptor to the chained acceptor. Assert that Accept
	// return nil UpfrontShutdown.
	dummy := &dummyAcceptor{}
	dummyID := chainedAcceptor.AddAcceptor(dummy)
	resp = chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Nil(t, resp.UpfrontShutdown)

	// Remove the dummyAcceptor from the chained acceptor and assert that
	// Accept return nil UpfrontShutdown.
	chainedAcceptor.RemoveAcceptor(dummyID)
	resp = chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Nil(t, resp.UpfrontShutdown)
}

// TestChainedAcceptorWithOpts verifies that the ChainedAcceptor will
// return valid UpfrontShutdown when specified.
func TestChainedAcceptorWithOpts(t *testing.T) {
	t.Parallel()

	// Create the chained acceptor.
	chainedAcceptor := NewChainedAcceptorWithOpts(testValidAddr,
		&chaincfg.RegressionNetParams)

	// Assert that calling Accept return valid UpfrontShutdown.
	req := &ChannelAcceptRequest{
		OpenChanMsg: &lnwire.OpenChannel{},
	}
	resp := chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)

	// Add a dummyAcceptor to the chained acceptor. Assert that Accept
	// return valid UpfrontShutdown.
	dummy := &dummyAcceptor{}
	dummyID := chainedAcceptor.AddAcceptor(dummy)
	resp = chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)

	// Remove the dummyAcceptor from the chained acceptor and assert that
	// Accept return valid UpfrontShutdown.
	chainedAcceptor.RemoveAcceptor(dummyID)
	resp = chainedAcceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.Equal(t, testAddr, resp.UpfrontShutdown)
}