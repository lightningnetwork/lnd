package channeldb

import (
	"bytes"
	"image/color"
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

// NodeAlias is a hex encoded UTF-8 string that may be displayed as an
// alternative to the node's ID. Notice that aliases are not unique and may be
// freely chosen by the node operators.
type NodeAlias [32]byte

// String returns a utf8 string representation of the alias bytes.
func (n NodeAlias) String() string {
	// Trim trailing zero-bytes for presentation
	return string(bytes.Trim(n[:], "\x00"))
}

type NodeAnnouncement struct {
	// Alias is used to customize node's appearance in maps and
	// graphs
	Alias NodeAlias

	// Color represent the hexadecimal value that node operators can assign
	// to their nodes. It's represented as a hex string.
	Color color.RGBA

	// NodeID is a public key which is used as node identification.
	NodeID [33]byte
}

// Sync performs a full database sync which writes the current up-to-date data
// within the struct to the database.
func (n *NodeAnnouncement) Sync(db *DB) error {
	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		nodeAnnBucket := tx.ReadWriteBucket(nodeAnnouncementBucket)
		if nodeAnnBucket == nil {
			return ErrNodeAnnBucketNotFound
		}

		return putNodeAnnouncement(nodeAnnBucket, n)
	}, func() {})
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

func (d *DB) PutNodeAnnouncement(pubkey [33]byte, alias [32]byte, color color.RGBA) error {
	nodeAnn := &NodeAnnouncement{
		Alias:  alias,
		Color:  color,
		NodeID: pubkey,
	}

	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		nodeAnnBucket := tx.ReadWriteBucket(nodeAnnouncementBucket)
		if nodeAnnBucket == nil {
			return ErrNodeAnnBucketNotFound
		}

		return putNodeAnnouncement(nodeAnnBucket, nodeAnn)

	}, func() {})
}

func putNodeAnnouncement(nodeAnnBucket kvdb.RwBucket, n *NodeAnnouncement) error {
	var b bytes.Buffer
	if err := serializeNodeAnnouncement(&b, n); err != nil {
		return err
	}

	nodePub := n.NodeID[:]
	return nodeAnnBucket.Put(nodePub, b.Bytes())
}

func serializeNodeAnnouncement(w io.Writer, n *NodeAnnouncement) error {
	// Serialize Alias
	if _, err := w.Write([]byte(n.Alias[:])); err != nil {
		return err
	}

	// Serialize Color
	// Write R
	if _, err := w.Write([]byte{n.Color.R}); err != nil {
		return err
	}
	// Write G
	if _, err := w.Write([]byte{n.Color.G}); err != nil {
		return err
	}
	// Write B
	if _, err := w.Write([]byte{n.Color.B}); err != nil {
		return err
	}

	// Serialize IdentityPub
	if _, err := w.Write(n.NodeID[:]); err != nil {
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
	nodeAnn.Alias = [32]byte(aliasBuf)

	// Read Color
	// colorBuf contains R, G, B, A (alpha), but the color.RGBA type only
	// expects R, G, B, so we need to slice it.
	colorBuf := make([]byte, 3)
	if _, err := io.ReadFull(r, colorBuf); err != nil {
		return nil, err
	}
	nodeAnn.Color = color.RGBA{colorBuf[0], colorBuf[1], colorBuf[2], 0}

	var pub [33]byte
	if _, err := io.ReadFull(r, pub[:]); err != nil {
		return nil, err
	}
	nodeAnn.NodeID = pub

	return nodeAnn, err

}
