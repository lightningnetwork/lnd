package channeldb

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestRepairLinkNodes tests that the RepairLinkNodes function correctly
// identifies and repairs missing link nodes for channels that exist in the
// database.
func TestRepairLinkNodes(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create a test channel and save it to the database.
	channel1 := createTestChannel(t, cdb)

	// Manually create a link node for the channel.
	linkNode1 := NewLinkNode(
		cdb.linkNodeDB, wire.MainNet, channel1.IdentityPub,
	)
	err = linkNode1.Sync()
	require.NoError(t, err, "unable to sync link node")

	// Verify that link node was created.
	fetchedLinkNode, err := cdb.linkNodeDB.FetchLinkNode(
		channel1.IdentityPub,
	)
	require.NoError(t, err, "link node should exist")
	require.NotNil(t, fetchedLinkNode, "link node should not be nil")

	// Now, manually delete one of the link nodes to simulate the race
	// condition scenario where a link node was incorrectly pruned.
	err = cdb.linkNodeDB.DeleteLinkNode(channel1.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	// Verify the link node is gone.
	_, err = cdb.linkNodeDB.FetchLinkNode(channel1.IdentityPub)
	require.ErrorIs(
		t, err, ErrNodeNotFound,
		"link node should be deleted",
	)

	// Now run the repair function with the correct network.
	err = cdb.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "repair should succeed")

	// Verify that the link node has been restored.
	repairedLinkNode, err := cdb.linkNodeDB.FetchLinkNode(
		channel1.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.NotNil(
		t, repairedLinkNode, "repaired link node should not be nil",
	)
	require.Equal(
		t, wire.MainNet, repairedLinkNode.Network,
		"repaired link node should have correct network",
	)

	// Run repair again - it should be idempotent and not fail.
	err = cdb.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "second repair should succeed")

	// Test with different network to ensure network parameter is used.
	err = cdb.linkNodeDB.DeleteLinkNode(channel1.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	err = cdb.RepairLinkNodes(wire.TestNet3)
	require.NoError(t, err, "repair with testnet should succeed")

	repairedLinkNode, err = cdb.linkNodeDB.FetchLinkNode(
		channel1.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.Equal(
		t, wire.TestNet3, repairedLinkNode.Network,
		"repaired link node should use provided network",
	)
}
