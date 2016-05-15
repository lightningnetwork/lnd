package channeldb

import (
	"fmt"

	"golang.org/x/crypto/ripemd160"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcutil"
)

var (
	idBucket = []byte("i")
)

// PutIdKey saves the hash160 of the public key used for our identity within
// the Lightning Network.
func (d *DB) PutIdKey(pkh []byte) error {
	return d.store.Update(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		bucket, err := tx.CreateBucketIfNotExists(idBucket)
		if err != nil {
			return err
		}

		return bucket.Put(identityKey, pkh)
	})
}

// GetIdKey returns the hash160 of the public key used for out identity within
// the Lightning Network as a p2pkh bitcoin address.
func (d *DB) GetIdAdr() (*btcutil.AddressPubKeyHash, error) {
	pkh := make([]byte, ripemd160.Size)
	err := d.store.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		bucket := tx.Bucket(idBucket)
		if bucket == nil {
			return fmt.Errorf("id bucket not created")
		}

		pkBytes := bucket.Get(identityKey)
		copy(pkh, pkBytes)

		return nil
	})
	if err != nil {
		return nil, err
	}

	log.Infof("identity key has length %d", len(pkh))
	return btcutil.NewAddressPubKeyHash(pkh, d.netParams)
}
