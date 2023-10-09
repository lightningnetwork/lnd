package channeldb

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var announcementNoncesBucket = []byte("announcement-nonces")

func (c *ChannelStateDB) SaveAnnouncementNonces(chanID lnwire.ChannelID, node,
	btc [musig2.PubNonceSize]byte) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	scratch := make([]byte, musig2.PubNonceSize*2)
	copy(scratch[:musig2.PubNonceSize], node[:])
	copy(scratch[musig2.PubNonceSize:], btc[:])

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

type AnnouncementNonces struct {
	Btc  [musig2.PubNonceSize]byte
	Node [musig2.PubNonceSize]byte
}

func (c *ChannelStateDB) GetAllAnnouncementNonces() (map[lnwire.ChannelID]*AnnouncementNonces, error) {

	m := make(map[lnwire.ChannelID]*AnnouncementNonces)
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

			copy(btc[:], v[:musig2.PubNonceSize])
			copy(node[:], v[musig2.PubNonceSize:])

			m[chanID] = &AnnouncementNonces{
				Btc:  btc,
				Node: node,
			}

			return nil
		})
	}, func() {
		m = make(map[lnwire.ChannelID]*AnnouncementNonces)
	})
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (c *ChannelStateDB) GetAnnouncementNonces(chanID lnwire.ChannelID) (
	*[musig2.PubNonceSize]byte, *[musig2.PubNonceSize]byte, error) {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	var node, btc [musig2.PubNonceSize]byte
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

		copy(node[:], noncesBytes[:musig2.PubNonceSize])
		copy(btc[:], noncesBytes[musig2.PubNonceSize:])

		return nil
	}, func() {})
	if err != nil {
		return nil, nil, err
	}

	return &node, &btc, nil
}

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
