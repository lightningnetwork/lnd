package channeldb

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

func TestLinkNodeEncodeDecode(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// First we'll create some initial data to use for populating our test
	// LinkNode instances.
	_, pub1 := btcec.PrivKeyFromBytes(key[:])
	_, pub2 := btcec.PrivKeyFromBytes(rev[:])
	addr1, err := net.ResolveTCPAddr("tcp", "10.0.0.1:9000")
	require.NoError(t, err, "unable to create test addr")
	addr2, err := net.ResolveTCPAddr("tcp", "10.0.0.2:9000")
	require.NoError(t, err, "unable to create test addr")

	// Create two fresh link node instances with the above dummy data, then
	// fully sync both instances to disk.
	node1 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub1, addr1)
	node2 := NewLinkNode(cdb.linkNodeDB, wire.TestNet3, pub2, addr2)
	if err := node1.Sync(); err != nil {
		t.Fatalf("unable to sync node: %v", err)
	}
	if err := node2.Sync(); err != nil {
		t.Fatalf("unable to sync node: %v", err)
	}

	// Fetch all current link nodes from the database, they should exactly
	// match the two created above.
	originalNodes := []*LinkNode{node2, node1}
	linkNodes, err := cdb.linkNodeDB.FetchAllLinkNodes()
	require.NoError(t, err, "unable to fetch nodes")
	for i, node := range linkNodes {
		if originalNodes[i].Network != node.Network {
			t.Fatalf("node networks don't match: expected %v, got %v",
				originalNodes[i].Network, node.Network)
		}

		originalPubkey := originalNodes[i].IdentityPub.SerializeCompressed()
		dbPubkey := node.IdentityPub.SerializeCompressed()
		if !bytes.Equal(originalPubkey, dbPubkey) {
			t.Fatalf("node pubkeys don't match: expected %x, got %x",
				originalPubkey, dbPubkey)
		}
		if originalNodes[i].LastSeen.Unix() != node.LastSeen.Unix() {
			t.Fatalf("last seen timestamps don't match: expected %v got %v",
				originalNodes[i].LastSeen.Unix(), node.LastSeen.Unix())
		}
		if originalNodes[i].Addresses[0].String() != node.Addresses[0].String() {
			t.Fatalf("addresses don't match: expected %v, got %v",
				originalNodes[i].Addresses, node.Addresses)
		}
	}

	// Next, we'll exercise the methods to append additional IP
	// addresses, and also to update the last seen time.
	if err := node1.UpdateLastSeen(time.Now()); err != nil {
		t.Fatalf("unable to update last seen: %v", err)
	}
	if err := node1.AddAddress(addr2); err != nil {
		t.Fatalf("unable to update addr: %v", err)
	}

	// Fetch the same node from the database according to its public key.
	node1DB, err := cdb.linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "unable to find node")

	// Both the last seen timestamp and the list of reachable addresses for
	// the node should be updated.
	if node1DB.LastSeen.Unix() != node1.LastSeen.Unix() {
		t.Fatalf("last seen timestamps don't match: expected %v got %v",
			node1.LastSeen.Unix(), node1DB.LastSeen.Unix())
	}
	if len(node1DB.Addresses) != 2 {
		t.Fatalf("wrong length for node1 addresses: expected %v, got %v",
			2, len(node1DB.Addresses))
	}
	if node1DB.Addresses[0].String() != addr1.String() {
		t.Fatalf("wrong address for node: expected %v, got %v",
			addr1.String(), node1DB.Addresses[0].String())
	}
	if node1DB.Addresses[1].String() != addr2.String() {
		t.Fatalf("wrong address for node: expected %v, got %v",
			addr2.String(), node1DB.Addresses[1].String())
	}
}

func TestDeleteLinkNode(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	_, pubKey := btcec.PrivKeyFromBytes(key[:])
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 1337,
	}
	linkNode := NewLinkNode(cdb.linkNodeDB, wire.TestNet3, pubKey, addr)
	if err := linkNode.Sync(); err != nil {
		t.Fatalf("unable to write link node to db: %v", err)
	}

	if _, err := cdb.linkNodeDB.FetchLinkNode(pubKey); err != nil {
		t.Fatalf("unable to find link node: %v", err)
	}

	if err := cdb.linkNodeDB.DeleteLinkNode(pubKey); err != nil {
		t.Fatalf("unable to delete link node from db: %v", err)
	}

	if _, err := cdb.linkNodeDB.FetchLinkNode(pubKey); err == nil {
		t.Fatal("should not have found link node in db, but did")
	}
}

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

// TestFindMissingLinkNodes tests the FindMissingLinkNodes method with various
// scenarios.
func TestFindMissingLinkNodes(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create three test public keys.
	_, pub1 := btcec.PrivKeyFromBytes(key[:])
	_, pub2 := btcec.PrivKeyFromBytes(rev[:])
	testKey := [32]byte{0x03}
	_, pub3 := btcec.PrivKeyFromBytes(testKey[:])

	// Test 1: All nodes missing (empty database).
	allPubs := []*btcec.PublicKey{pub1, pub2, pub3}
	missing, err := cdb.linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 3, "all nodes should be missing")

	// Test 2: Create one link node, verify only 2 are missing.
	node1 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub1)
	err = node1.Sync()
	require.NoError(t, err, "unable to sync link node")

	missing, err = cdb.linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 2, "two nodes should be missing")
	require.Contains(t, missing, pub2, "pub2 should be missing")
	require.Contains(t, missing, pub3, "pub3 should be missing")
	require.NotContains(t, missing, pub1, "pub1 should exist")

	// Test 3: Create remaining nodes, verify none are missing.
	node2 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub2)
	err = node2.Sync()
	require.NoError(t, err, "unable to sync link node")

	node3 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub3)
	err = node3.Sync()
	require.NoError(t, err, "unable to sync link node")

	missing, err = cdb.linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 0, "no nodes should be missing")

	// Test 4: Use with a provided transaction.
	err = cdb.linkNodeDB.DeleteLinkNode(pub2)
	require.NoError(t, err, "unable to delete link node")

	backend := fullDB.ChannelStateDB().backend
	err = kvdb.View(backend, func(tx kvdb.RTx) error {
		missing, err := cdb.linkNodeDB.FindMissingLinkNodes(
			tx, allPubs,
		)
		require.NoError(t, err, "FindMissingLinkNodes should succeed")
		require.Len(t, missing, 1, "one node should be missing")
		require.Contains(t, missing, pub2, "pub2 should be missing")

		return nil
	}, func() {})
	require.NoError(t, err, "transaction should succeed")

	// Test 5: Empty input list.
	missing, err = cdb.linkNodeDB.FindMissingLinkNodes(nil, nil)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 0, "no nodes should be missing for empty input")
}

// TestCreateLinkNodes tests the CreateLinkNodes method with various scenarios.
func TestCreateLinkNodes(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create three test public keys and link nodes.
	_, pub1 := btcec.PrivKeyFromBytes(key[:])
	_, pub2 := btcec.PrivKeyFromBytes(rev[:])
	testKey := [32]byte{0x03}
	_, pub3 := btcec.PrivKeyFromBytes(testKey[:])

	node1 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub1)
	node2 := NewLinkNode(cdb.linkNodeDB, wire.TestNet3, pub2)
	node3 := NewLinkNode(cdb.linkNodeDB, wire.SimNet, pub3)

	// Test 1: Create multiple link nodes at once with nil transaction.
	nodesToCreate := []*LinkNode{node1, node2, node3}
	err = cdb.linkNodeDB.CreateLinkNodes(nil, nodesToCreate)
	require.NoError(t, err, "CreateLinkNodes should succeed")

	// Verify all nodes were created correctly.
	fetchedNode1, err := cdb.linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "node1 should exist")
	require.Equal(t, wire.MainNet, fetchedNode1.Network,
		"node1 should have correct network")

	fetchedNode2, err := cdb.linkNodeDB.FetchLinkNode(pub2)
	require.NoError(t, err, "node2 should exist")
	require.Equal(t, wire.TestNet3, fetchedNode2.Network,
		"node2 should have correct network")

	fetchedNode3, err := cdb.linkNodeDB.FetchLinkNode(pub3)
	require.NoError(t, err, "node3 should exist")
	require.Equal(t, wire.SimNet, fetchedNode3.Network,
		"node3 should have correct network")

	// Test 2: Create nodes within a provided transaction.
	err = cdb.linkNodeDB.DeleteLinkNode(pub2)
	require.NoError(t, err, "unable to delete link node")

	// Verify node2 is deleted.
	_, err = cdb.linkNodeDB.FetchLinkNode(pub2)
	require.ErrorIs(t, err, ErrNodeNotFound, "node2 should be deleted")

	// Recreate node2 using a provided transaction.
	backend := fullDB.ChannelStateDB().backend
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		return cdb.linkNodeDB.CreateLinkNodes(tx, []*LinkNode{node2})
	}, func() {})
	require.NoError(t, err, "transaction should succeed")

	// Verify node2 was recreated.
	fetchedNode2, err = cdb.linkNodeDB.FetchLinkNode(pub2)
	require.NoError(t, err, "node2 should exist after recreation")
	require.Equal(t, wire.TestNet3, fetchedNode2.Network,
		"node2 should have correct network")

	// Test 3: Creating nodes that already exist should succeed
	// (idempotent behavior).
	err = cdb.linkNodeDB.CreateLinkNodes(nil, nodesToCreate)
	require.NoError(t, err, "recreating existing nodes should succeed")

	// Verify nodes still exist with correct data.
	fetchedNode1, err = cdb.linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "node1 should still exist")
	require.Equal(t, wire.MainNet, fetchedNode1.Network,
		"node1 should still have correct network")

	// Test 4: Empty input list.
	err = cdb.linkNodeDB.CreateLinkNodes(nil, nil)
	require.NoError(
		t, err, "CreateLinkNodes with empty list should succeed",
	)

	// Test 5: Create single node.
	testKey4 := [32]byte{0x04}
	_, pub4 := btcec.PrivKeyFromBytes(testKey4[:])
	node4 := NewLinkNode(cdb.linkNodeDB, wire.MainNet, pub4)

	err = cdb.linkNodeDB.CreateLinkNodes(nil, []*LinkNode{node4})
	require.NoError(
		t, err, "CreateLinkNodes with single node should succeed",
	)

	fetchedNode4, err := cdb.linkNodeDB.FetchLinkNode(pub4)
	require.NoError(t, err, "node4 should exist")
	require.Equal(t, wire.MainNet, fetchedNode4.Network,
		"node4 should have correct network")
}
