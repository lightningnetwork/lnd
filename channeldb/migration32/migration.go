package migration32

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

// waitingProofsBucketKey byte string name of the waiting proofs store.
var waitingProofsBucketKey = []byte("waitingproofs")

// MigrateWaitingProofStore migrates the waiting proof store so that all entries
// are prefixed with a type byte.
func MigrateWaitingProofStore(tx kvdb.RwTx) error {
	log.Infof("Migrating waiting proof store")

	bucket := tx.ReadWriteBucket(waitingProofsBucketKey)

	// If the bucket does not exist yet, then there are no entries to
	// migrate.
	if bucket == nil {
		return nil
	}

	return bucket.ForEach(func(k, v []byte) error {
		// Skip buckets fields.
		if v == nil {
			return nil
		}

		// Read in the waiting proof using the legacy decoding method.
		var proof WaitingProof
		if err := proof.LegacyDecode(bytes.NewReader(v)); err != nil {
			return err
		}

		// Do sanity check to ensure that the proof key is the same as
		// the key used to store the proof.
		key := proof.Key()
		if !bytes.Equal(key[:], k) {
			return fmt.Errorf("proof key (%x) does not match "+
				"the key used to store the proof: %x", key, k)
		}

		// Re-encode the proof using the new, type-prefixed encoding.
		var b bytes.Buffer
		err := proof.UpdatedEncode(&b)
		if err != nil {
			return err
		}

		return bucket.Put(k, b.Bytes())
	})
}
