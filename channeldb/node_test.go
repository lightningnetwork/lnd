package channeldb

import (
	"image/color"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
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

	nodeAnn := &NodeAnnouncement{
		Alias:  alias,
		Color:  color,
		NodeID: [33]byte(pub.SerializeCompressed()),
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

}
