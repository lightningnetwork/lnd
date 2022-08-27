package channeldb

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
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
