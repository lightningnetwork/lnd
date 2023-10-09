package channeldb

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// announcementNoncesBucket is a top level bucket with the following
	// structure: channel_id => node_nonce || bitcoin_nonce
	announcementNoncesBucket = []byte("announcement-nonces")
)

// AnnouncementNonces holds the nonces used during the creating of a
// ChannelAnnouncement2.
type AnnouncementNonces struct {
	Btc  [musig2.PubNonceSize]byte
	Node [musig2.PubNonceSize]byte
}

// SaveAnnouncementNonces persist the given announcement nonces.
func (c *ChannelStateDB) SaveAnnouncementNonces(chanID lnwire.ChannelID,
	nonces *AnnouncementNonces) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	scratch := make([]byte, musig2.PubNonceSize*2)
	copy(scratch[:musig2.PubNonceSize], nonces.Node[:])
	copy(scratch[musig2.PubNonceSize:], nonces.Btc[:])

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(
			announcementNoncesBucket,
		)
		if err != nil {
			return err
		}

		return bucket.Put(chanIDCopy, scratch)
	}, func() {})
}

// GetAllAnnouncementNonces returns all the announcement nonce pairs currently
// stored in the DB.
func (c *ChannelStateDB) GetAllAnnouncementNonces() (
	map[lnwire.ChannelID]AnnouncementNonces, error) {

	m := make(map[lnwire.ChannelID]AnnouncementNonces)
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(announcementNoncesBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			if len(k) != 32 {
				return fmt.Errorf("invalid chan ID key")
			}
			if len(v) != musig2.PubNonceSize*2 {
				return fmt.Errorf("wrong number of bytes")
			}

			var chanID lnwire.ChannelID
			copy(chanID[:], k)

			var btc, node [musig2.PubNonceSize]byte

			copy(node[:], v[:musig2.PubNonceSize])
			copy(btc[:], v[musig2.PubNonceSize:])

			m[chanID] = AnnouncementNonces{
				Btc:  btc,
				Node: node,
			}

			return nil
		})
	}, func() {
		m = make(map[lnwire.ChannelID]AnnouncementNonces)
	})
	if err != nil {
		return nil, err
	}

	return m, nil
}

// GetAnnouncementNonces fetches the announcement nonces for the given channel
// ID.
func (c *ChannelStateDB) GetAnnouncementNonces(chanID lnwire.ChannelID) (
	*AnnouncementNonces, error) {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	var nonces AnnouncementNonces
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(announcementNoncesBucket)
		if bucket == nil {
			return ErrChannelNotFound
		}

		noncesBytes := bucket.Get(chanIDCopy)
		if noncesBytes == nil {
			return ErrChannelNotFound
		}

		if len(noncesBytes) != musig2.PubNonceSize*2 {
			return fmt.Errorf("wrong number of bytes")
		}

		copy(nonces.Node[:], noncesBytes[:musig2.PubNonceSize])
		copy(nonces.Btc[:], noncesBytes[musig2.PubNonceSize:])

		return nil
	}, func() {
		nonces = AnnouncementNonces{}
	})
	if err != nil {
		return nil, err
	}

	return &nonces, nil
}

// DeleteAnnouncementNonces deletes the announcement nonce pair stored under
// the given channel ID key if an entry exists.
func (c *ChannelStateDB) DeleteAnnouncementNonces(
	chanID lnwire.ChannelID) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(
			announcementNoncesBucket,
		)
		if bucket == nil {
			return nil
		}

		return bucket.Delete(chanIDCopy)
	}, func() {})
}
