package channeldb

import (
	"image/color"
	"net"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestNodeAnnouncementEncodeDecode(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	// We'll start by creating initial data to use for populating our test
	// node announcement instance
	var alias [32]byte
	copy(alias[:], []byte("alice"))
	color := color.RGBA{255, 255, 255, 0}
	_, pub := btcec.PrivKeyFromBytes(key[:])
	address := []net.Addr{testAddr}
	features := lnwire.RawFeatureVector{}

	nodeAnn := &NodeAnnouncement{
		Alias:     alias,
		Color:     color,
		NodeID:    [33]byte(pub.SerializeCompressed()),
		Addresses: address,
		Features:  &features,
	}
	if err := nodeAnn.Sync(fullDB); err != nil {
		t.Fatalf("unable to sync node announcement: %v", err)
	}

	// Fetch the current node announcement from the database, it should
	// match the one we just persisted
	persistedNodeAnn, err := fullDB.FetchNodeAnnouncement(pub)
	require.NoError(t, err, "unable to fetch node announcement")
	if nodeAnn.Alias != persistedNodeAnn.Alias {
		t.Fatalf("node aliases don't match: expected %v, got %v",
			nodeAnn.Alias.String(), persistedNodeAnn.Alias.String())
	}

	if nodeAnn.Color != persistedNodeAnn.Color {
		t.Fatalf("node colors don't match: expected %v, got %v",
			nodeAnn.Color, persistedNodeAnn.Color)
	}

	if nodeAnn.NodeID != persistedNodeAnn.NodeID {
		t.Fatalf("node nodeIds don't match: expected %v, got %v",
			nodeAnn.NodeID, persistedNodeAnn.NodeID)
	}

	// Verify that the addresses of the node announcements are the same.
	if !reflect.DeepEqual(nodeAnn.Addresses, persistedNodeAnn.Addresses) {
		t.Fatalf("node addresses don't match: expected %v, got %v",
			nodeAnn.Addresses, persistedNodeAnn.Addresses)
	}

}
