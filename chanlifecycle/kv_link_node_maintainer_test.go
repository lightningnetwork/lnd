package chanlifecycle_test

import (
	"net"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/linknode"
	"github.com/stretchr/testify/require"
)

// TestRepairLinkNodes tests that RepairLinkNodes identifies and repairs
// missing link nodes for channels that exist in the database.
func TestRepairLinkNodes(t *testing.T) {
	t.Parallel()

	fixture := newKVChannelTestContext(t)

	channel := newKVTestChannel(t, fixture.chanStore)
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	err := fixture.lifecycle.SyncPendingChannel(
		channel, addr, uint32(100),
	)
	require.NoError(t, err, "unable to sync pending channel")

	fetchedLinkNode, err := fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "link node should exist")
	require.NotNil(t, fetchedLinkNode, "link node should not be nil")

	err = fixture.linkNodeStore.DeleteLinkNode(channel.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	_, err = fixture.linkNodeStore.FetchLinkNode(channel.IdentityPub)
	require.ErrorIs(t, err, linknode.ErrNodeNotFound)

	err = fixture.maintainer.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "repair should succeed")

	repairedLinkNode, err := fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.NotNil(
		t, repairedLinkNode, "repaired link node should not be nil",
	)
	require.Equal(t, wire.MainNet, repairedLinkNode.Network)

	err = fixture.maintainer.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "second repair should succeed")

	err = fixture.linkNodeStore.DeleteLinkNode(channel.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	err = fixture.maintainer.RepairLinkNodes(wire.TestNet3)
	require.NoError(t, err, "repair with testnet should succeed")

	repairedLinkNode, err = fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.Equal(t, wire.TestNet3, repairedLinkNode.Network)
}
