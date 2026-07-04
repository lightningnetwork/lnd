package linknode

import (
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (
	key = [32]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	rev = [32]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x3e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
)

func TestMain(m *testing.M) {
	kvdb.RunTests(m)
}

func makeTestLinkNodeDB(t *testing.T) (*LinkNodeDB, kvdb.Backend) {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "cdb")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(NodeInfoBucket)

		return err
	}, func() {})
	require.NoError(t, err)

	return NewDB(backend), backend
}

func TestLinkNodeEncodeDecode(t *testing.T) {
	t.Parallel()

	linkNodeDB, _ := makeTestLinkNodeDB(t)

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
	node1 := NewLinkNode(linkNodeDB, wire.MainNet, pub1, addr1)
	node2 := NewLinkNode(linkNodeDB, wire.TestNet3, pub2, addr2)
	if err := node1.Sync(); err != nil {
		t.Fatalf("unable to sync node: %v", err)
	}
	if err := node2.Sync(); err != nil {
		t.Fatalf("unable to sync node: %v", err)
	}

	// Fetch all current link nodes from the database, they should exactly
	// match the two created above.
	originalNodes := map[string]*LinkNode{
		string(pub1.SerializeCompressed()): node1,
		string(pub2.SerializeCompressed()): node2,
	}
	linkNodes, err := linkNodeDB.FetchAllLinkNodes()
	require.NoError(t, err, "unable to fetch nodes")
	for _, node := range linkNodes {
		dbPubkey := node.IdentityPub.SerializeCompressed()
		originalNode, ok := originalNodes[string(dbPubkey)]
		require.True(t, ok, "unexpected node: %x", dbPubkey)

		require.Equal(t, originalNode.Network, node.Network)

		originalPubkey := originalNode.IdentityPub.SerializeCompressed()
		require.Equal(t, originalPubkey, dbPubkey)

		originalLastSeen := originalNode.LastSeen.Unix()
		nodeLastSeen := node.LastSeen.Unix()
		require.Equal(t, originalLastSeen, nodeLastSeen)

		originalAddr := originalNode.Addresses[0].String()
		nodeAddr := node.Addresses[0].String()
		require.Equal(t, originalAddr, nodeAddr)
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
	node1DB, err := linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "unable to find node")

	// Both the last seen timestamp and the list of reachable addresses for
	// the node should be updated.
	if node1DB.LastSeen.Unix() != node1.LastSeen.Unix() {
		t.Fatalf("last seen timestamps don't match: expected %v got %v",
			node1.LastSeen.Unix(), node1DB.LastSeen.Unix())
	}
	require.Len(t, node1DB.Addresses, 2)
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

	linkNodeDB, _ := makeTestLinkNodeDB(t)

	_, pubKey := btcec.PrivKeyFromBytes(key[:])
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 1337,
	}
	linkNode := NewLinkNode(linkNodeDB, wire.TestNet3, pubKey, addr)
	if err := linkNode.Sync(); err != nil {
		t.Fatalf("unable to write link node to db: %v", err)
	}

	if _, err := linkNodeDB.FetchLinkNode(pubKey); err != nil {
		t.Fatalf("unable to find link node: %v", err)
	}

	if err := linkNodeDB.DeleteLinkNode(pubKey); err != nil {
		t.Fatalf("unable to delete link node from db: %v", err)
	}

	if _, err := linkNodeDB.FetchLinkNode(pubKey); err == nil {
		t.Fatal("should not have found link node in db, but did")
	}
}

// TestFindMissingLinkNodes tests the FindMissingLinkNodes method with various
// scenarios.
func TestFindMissingLinkNodes(t *testing.T) {
	t.Parallel()

	linkNodeDB, backend := makeTestLinkNodeDB(t)

	// Create three test public keys.
	_, pub1 := btcec.PrivKeyFromBytes(key[:])
	_, pub2 := btcec.PrivKeyFromBytes(rev[:])
	testKey := [32]byte{0x03}
	_, pub3 := btcec.PrivKeyFromBytes(testKey[:])

	// Test 1: All nodes missing (empty database).
	allPubs := []*btcec.PublicKey{pub1, pub2, pub3}
	missing, err := linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 3, "all nodes should be missing")

	// Test 2: Create one link node, verify only 2 are missing.
	node1 := NewLinkNode(linkNodeDB, wire.MainNet, pub1)
	err = node1.Sync()
	require.NoError(t, err, "unable to sync link node")

	missing, err = linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 2, "two nodes should be missing")
	require.Contains(t, missing, pub2, "pub2 should be missing")
	require.Contains(t, missing, pub3, "pub3 should be missing")
	require.NotContains(t, missing, pub1, "pub1 should exist")

	// Test 3: Create remaining nodes, verify none are missing.
	node2 := NewLinkNode(linkNodeDB, wire.MainNet, pub2)
	err = node2.Sync()
	require.NoError(t, err, "unable to sync link node")

	node3 := NewLinkNode(linkNodeDB, wire.MainNet, pub3)
	err = node3.Sync()
	require.NoError(t, err, "unable to sync link node")

	missing, err = linkNodeDB.FindMissingLinkNodes(nil, allPubs)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 0, "no nodes should be missing")

	// Test 4: Use with a provided transaction.
	err = linkNodeDB.DeleteLinkNode(pub2)
	require.NoError(t, err, "unable to delete link node")

	err = kvdb.View(backend, func(tx kvdb.RTx) error {
		missing, err := linkNodeDB.FindMissingLinkNodes(
			tx, allPubs,
		)
		require.NoError(t, err, "FindMissingLinkNodes should succeed")
		require.Len(t, missing, 1, "one node should be missing")
		require.Contains(t, missing, pub2, "pub2 should be missing")

		return nil
	}, func() {})
	require.NoError(t, err, "transaction should succeed")

	// Test 5: Empty input list.
	missing, err = linkNodeDB.FindMissingLinkNodes(nil, nil)
	require.NoError(t, err, "FindMissingLinkNodes should succeed")
	require.Len(t, missing, 0, "no nodes should be missing for empty input")
}

// TestCreateLinkNodes tests the CreateLinkNodes method with various scenarios.
func TestCreateLinkNodes(t *testing.T) {
	t.Parallel()

	linkNodeDB, backend := makeTestLinkNodeDB(t)

	// Create three test public keys and link nodes.
	_, pub1 := btcec.PrivKeyFromBytes(key[:])
	_, pub2 := btcec.PrivKeyFromBytes(rev[:])
	testKey := [32]byte{0x03}
	_, pub3 := btcec.PrivKeyFromBytes(testKey[:])

	node1 := NewLinkNode(linkNodeDB, wire.MainNet, pub1)
	node2 := NewLinkNode(linkNodeDB, wire.TestNet3, pub2)
	node3 := NewLinkNode(linkNodeDB, wire.SimNet, pub3)

	// Test 1: Create multiple link nodes at once with nil transaction.
	nodesToCreate := []*LinkNode{node1, node2, node3}
	err := linkNodeDB.CreateLinkNodes(nil, nodesToCreate)
	require.NoError(t, err, "CreateLinkNodes should succeed")

	// Verify all nodes were created correctly.
	fetchedNode1, err := linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "node1 should exist")
	require.Equal(t, wire.MainNet, fetchedNode1.Network,
		"node1 should have correct network")

	fetchedNode2, err := linkNodeDB.FetchLinkNode(pub2)
	require.NoError(t, err, "node2 should exist")
	require.Equal(t, wire.TestNet3, fetchedNode2.Network,
		"node2 should have correct network")

	fetchedNode3, err := linkNodeDB.FetchLinkNode(pub3)
	require.NoError(t, err, "node3 should exist")
	require.Equal(t, wire.SimNet, fetchedNode3.Network,
		"node3 should have correct network")

	// Test 2: Create nodes within a provided transaction.
	err = linkNodeDB.DeleteLinkNode(pub2)
	require.NoError(t, err, "unable to delete link node")

	// Verify node2 is deleted.
	_, err = linkNodeDB.FetchLinkNode(pub2)
	require.ErrorIs(t, err, ErrNodeNotFound, "node2 should be deleted")

	// Recreate node2 using a provided transaction.
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		return linkNodeDB.CreateLinkNodes(tx, []*LinkNode{node2})
	}, func() {})
	require.NoError(t, err, "transaction should succeed")

	// Verify node2 was recreated.
	fetchedNode2, err = linkNodeDB.FetchLinkNode(pub2)
	require.NoError(t, err, "node2 should exist after recreation")
	require.Equal(t, wire.TestNet3, fetchedNode2.Network,
		"node2 should have correct network")

	// Test 3: Creating nodes that already exist should succeed
	// (idempotent behavior).
	err = linkNodeDB.CreateLinkNodes(nil, nodesToCreate)
	require.NoError(t, err, "recreating existing nodes should succeed")

	// Verify nodes still exist with correct data.
	fetchedNode1, err = linkNodeDB.FetchLinkNode(pub1)
	require.NoError(t, err, "node1 should still exist")
	require.Equal(t, wire.MainNet, fetchedNode1.Network,
		"node1 should still have correct network")

	// Test 4: Empty input list.
	err = linkNodeDB.CreateLinkNodes(nil, nil)
	require.NoError(
		t, err, "CreateLinkNodes with empty list should succeed",
	)

	// Test 5: Create single node.
	testKey4 := [32]byte{0x04}
	_, pub4 := btcec.PrivKeyFromBytes(testKey4[:])
	node4 := NewLinkNode(linkNodeDB, wire.MainNet, pub4)

	err = linkNodeDB.CreateLinkNodes(nil, []*LinkNode{node4})
	require.NoError(
		t, err, "CreateLinkNodes with single node should succeed",
	)

	fetchedNode4, err := linkNodeDB.FetchLinkNode(pub4)
	require.NoError(t, err, "node4 should exist")
	require.Equal(t, wire.MainNet, fetchedNode4.Network,
		"node4 should have correct network")
}
