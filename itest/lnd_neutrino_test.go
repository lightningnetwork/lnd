package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testNeutrino checks that the neutrino sub-server can fetch compact
// block filters, server status and connect to a connected peer.
func testNeutrino(ht *lntest.HarnessTest) {
	if !ht.IsNeutrinoBackend() {
		ht.Skipf("skipping test for non neutrino backends")
	}

	alice := ht.NewNode("Alice", nil)

	// Check if the neutrino sub server is running.
	statusRes := alice.RPC.Status(nil)
	require.True(ht, statusRes.Active)
	require.Len(ht, statusRes.Peers, 1, "unable to find a peer")

	// Request the compact block filter of the best block.
	cFilterReq := &neutrinorpc.GetCFilterRequest{
		Hash: statusRes.GetBlockHash(),
	}
	alice.RPC.GetCFilter(cFilterReq)

	// Try to reconnect to a connected peer.
	addPeerReq := &neutrinorpc.AddPeerRequest{
		PeerAddrs: statusRes.Peers[0],
	}
	alice.RPC.AddPeer(addPeerReq)
}
