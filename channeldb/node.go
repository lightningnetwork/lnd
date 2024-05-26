package channeldb

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// nodeAnnouncementBucket stores announcement config pertaining to node.
	// This bucket allows one to query for persisted node announcement
	// config and use it when starting orrestarting a node
	nodeAnnouncementBucket = []byte("nab")
)

type NodeAnnouncement struct {
	// Alias indicate the human readable name that a node operator can
	// assign to their node for better readability and easier identification
	Alias string

	// Color represent the hexadecimal value that node operators can assign
	// to their nodes. It's represented as a hex string.
	Color string

	// IdentityPub is the node's current identity public key. Any
	// channel/topology related information received by this node MUST be
	// signed by this public key.
	IdentityPub *btcec.PublicKey
}

// FetchNodeAnnouncement attempts to lookup the data for NodeAnnouncement based
// on a target identity public key. If a particular NodeAnnouncement for the
// passed identity public key cannot be found, then returns ErrNodeAnnNotFound
func (d *DB) FetchNodeAnnouncement(identity *btcec.PublicKey) (*NodeAnnouncement, error) {
	var nodeAnnouncement *NodeAnnouncement
	err := kvdb.View(d, func(tx kvdb.RTx) error {
		nodeAnn, err := fetchNodeAnnouncement(tx, identity)
		if err != nil {
			return err
		}

		nodeAnnouncement = nodeAnn
		return nil
	}, func() {
		nodeAnnouncement = nil
	})

	return nodeAnnouncement, err
}

func fetchNodeAnnouncement(tx kvdb.RTx, targetPub *btcec.PublicKey) (*NodeAnnouncement, error) {
	// First fetch the bucket for storing node announcement, bailing out
	// early if it hasn't been created yet.
	nodeAnnBucket := tx.ReadBucket(nodeAnnouncementBucket)
	if nodeAnnBucket == nil {
		return nil, ErrNodeAnnBucketNotFound
	}

	// If a node announcement for that particular public key cannot be
	// located, then exit early with ErrNodeAnnNotFound
	pubkey := targetPub.SerializeCompressed()
	nodeAnnBytes := nodeAnnBucket.Get(pubkey)
	if nodeAnnBytes == nil {
		return nil, ErrNodeAnnNotFound
	}

	// FInally, decode and allocate a fresh NodeAnnouncement object to be
	// returned to the caller
	nodeAnnReader := bytes.NewReader(nodeAnnBytes)
	return deserializeNodeAnnouncement(nodeAnnReader)

}

func (d *DB) PutNodeAnnouncement(pubkey *btcec.PublicKey, alias, color string) error {
	nodeAnn := &NodeAnnouncement{
		Alias:       alias,
		IdentityPub: pubkey,
		Color:       color,
	}

	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		nodeAnnouncements := tx.ReadWriteBucket(nodeAnnouncementBucket)

		nodeAnnBucket, err := nodeAnnouncements.CreateBucketIfNotExists(pubkey.SerializeCompressed())
		if err != nil {
			return err
		}

		var b bytes.Buffer
		if err := serializeNodeAnnouncement(&b, nodeAnn); err != nil {
			return err
		}

		err = nodeAnnBucket.Put(pubkey.SerializeCompressed(), b.Bytes())
		if err != nil {
			return err
		}

		return nil

	}, func() {})
}

func serializeNodeAnnouncement(w io.Writer, n *NodeAnnouncement) error {
	// Serialize Alias
	if _, err := w.Write([]byte(n.Alias + "\x00")); err != nil {
		return err
	}

	// Serialize Color
	if _, err := w.Write([]byte(n.Color + "\x00")); err != nil {
		return err
	}

	// Serialize IdentityPub
	serializedID := n.IdentityPub.SerializeCompressed()
	if _, err := w.Write(serializedID); err != nil {
		return err
	}

	return nil
}

func deserializeNodeAnnouncement(r io.Reader) (*NodeAnnouncement, error) {
	var err error
	nodeAnn := &NodeAnnouncement{}

	// Read Alias
	aliasBuf := make([]byte, 32)
	if _, err := io.ReadFull(r, aliasBuf); err != nil {
		return nil, err
	}
	nodeAnn.Alias = string(bytes.TrimRight(aliasBuf, "\x00"))

	// Read Color
	colorBuf := make([]byte, 8)
	if _, err := io.ReadFull(r, colorBuf); err != nil {
		return nil, err
	}
	nodeAnn.Color = string(bytes.TrimRight(colorBuf, "\x00"))

	var pub [33]byte
	if _, err := io.ReadFull(r, pub[:]); err != nil {
		return nil, err
	}
	nodeAnn.IdentityPub, err = btcec.ParsePubKey(pub[:])
	if err != nil {
		return nil, err
	}

	return nodeAnn, err

}
