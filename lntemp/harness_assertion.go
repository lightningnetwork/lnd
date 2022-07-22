package lntemp

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// WaitForBlockchainSync waits until the node is synced to chain.
func (h *HarnessTest) WaitForBlockchainSync(hn *node.HarnessNode) {
	err := wait.NoError(func() error {
		resp := hn.RPC.GetInfo()
		if resp.SyncedToChain {
			return nil
		}
		return fmt.Errorf("%s is not synced to chain", hn.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "timeout waiting for blockchain sync")
}

// AssertPeerConnected asserts that the given node b is connected to a.
func (h *HarnessTest) AssertPeerConnected(a, b *node.HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := a.RPC.ListPeers()

		// If node B is seen in the ListPeers response from node A,
		// then we can return true as the connection has been fully
		// established.
		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return nil
			}
		}

		return fmt.Errorf("%s not found in %s's ListPeers",
			b.Name(), a.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "unable to connect %s to %s, got error: "+
		"peers not connected within %v seconds",
		a.Name(), b.Name(), DefaultTimeout)
}

// ConnectNodes creates a connection between the two nodes and asserts the
// connection is succeeded.
func (h *HarnessTest) ConnectNodes(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
	}
	a.RPC.ConnectPeer(req)
	h.AssertPeerConnected(a, b)
}

// DisconnectNodes disconnects the given two nodes and asserts the
// disconnection is succeeded. The request is made from node a and sent to node
// b.
func (h *HarnessTest) DisconnectNodes(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()
	a.RPC.DisconnectPeer(bobInfo.IdentityPubkey)
}

// EnsureConnected will try to connect to two nodes, returning no error if they
// are already connected. If the nodes were not connected previously, this will
// behave the same as ConnectNodes. If a pending connection request has already
// been made, the method will block until the two nodes appear in each other's
// peers list, or until the DefaultTimeout expires.
func (h *HarnessTest) EnsureConnected(a, b *node.HarnessNode) {
	// errConnectionRequested is used to signal that a connection was
	// requested successfully, which is distinct from already being
	// connected to the peer.
	errConnectionRequested := "connection request in progress"

	tryConnect := func(a, b *node.HarnessNode) error {
		bInfo := b.RPC.GetInfo()

		req := &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: bInfo.IdentityPubkey,
				Host:   b.Cfg.P2PAddr(),
			},
		}

		ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
		defer cancel()

		_, err := a.RPC.LN.ConnectPeer(ctxt, req)

		// Request was successful.
		if err == nil {
			return nil
		}

		// If the connection is in process, we return no error.
		if strings.Contains(err.Error(), errConnectionRequested) {
			return nil
		}

		// If the two are already connected, we return early with no
		// error.
		if strings.Contains(err.Error(), "already connected to peer") {
			return nil
		}

		// We may get connection refused error if we happens to be in
		// the middle of a previous node disconnection, e.g., a restart
		// from one of the nodes.
		if strings.Contains(err.Error(), "connection refused") {
			return nil
		}

		return err
	}

	// Return any critical errors returned by either alice or bob.
	require.NoError(h, tryConnect(a, b), "connection failed between %s "+
		"and %s", a.Cfg.Name, b.Cfg.Name)

	// When Alice and Bob each makes a connection to the other side at the
	// same time, it's likely neither connections could succeed. Bob's
	// connection will be canceled by Alice since she has an outbound
	// connection to Bob already, and same happens to Alice's. Thus the two
	// connections cancel each other out.
	// TODO(yy): move this back when the above issue is fixed.
	// require.NoError(h, tryConnect(b, a), "connection failed between %s "+
	// 	"and %s", a.Cfg.Name, b.Cfg.Name)

	// Otherwise one or both requested a connection, so we wait for the
	// peers lists to reflect the connection.
	h.AssertPeerConnected(a, b)
	h.AssertPeerConnected(b, a)
}
