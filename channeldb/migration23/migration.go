package migration23

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// paymentsRootBucket is the name of the top-level bucket within the
	// database that stores all data related to payments.
	paymentsRootBucket = []byte("payments-root-bucket")

	// paymentHtlcsBucket is a bucket where we'll store the information
	// about the HTLCs that were attempted for a payment.
	paymentHtlcsBucket = []byte("payment-htlcs-bucket")

	// oldAttemptInfoKey is a key used in a HTLC's sub-bucket to store the
	// info about the attempt that was done for the HTLC in question.
	oldAttemptInfoKey = []byte("htlc-attempt-info")

	// oldSettleInfoKey is a key used in a HTLC's sub-bucket to store the
	// settle info, if any.
	oldSettleInfoKey = []byte("htlc-settle-info")

	// oldFailInfoKey is a key used in a HTLC's sub-bucket to store
	// failure information, if any.
	oldFailInfoKey = []byte("htlc-fail-info")

	// htlcAttemptInfoKey is the key used as the prefix of an HTLC attempt
	// to store the info about the attempt that was done for the HTLC in
	// question. The HTLC attempt ID is concatenated at the end.
	htlcAttemptInfoKey = []byte("ai")

	// htlcSettleInfoKey is the key used as the prefix of an HTLC attempt
	// settle info, if any. The HTLC attempt ID is concatenated at the end.
	htlcSettleInfoKey = []byte("si")

	// htlcFailInfoKey is the key used as the prefix of an HTLC attempt
	// failure information, if any.The  HTLC attempt ID is concatenated at
	// the end.
	htlcFailInfoKey = []byte("fi")
)

// htlcBucketKey creates a composite key from prefix and id where the result is
// simply the two concatenated. This is the exact copy from payments.go.
func htlcBucketKey(prefix, id []byte) []byte {
	key := make([]byte, len(prefix)+len(id))
	copy(key, prefix)
	copy(key[len(prefix):], id)
	return key
}

// MigrateHtlcAttempts will gather all htlc-attempt-info's, htlcs-settle-info's
// and htlcs-fail-info's from the attempt ID buckes and re-store them using the
// flattened keys to each payment's payment-htlcs-bucket.
func MigrateHtlcAttempts(tx kvdb.RwTx) error {
	payments := tx.ReadWriteBucket(paymentsRootBucket)
	if payments == nil {
		return nil
	}

	// Collect all payment hashes so we can migrate payments one-by-one to
	// avoid any bugs bbolt might have when invalidating cursors.
	// For 100 million payments, this would need about 3 GiB memory so we
	// should hopefully be fine for very large nodes too.
	var paymentHashes []string
	if err := payments.ForEach(func(hash, v []byte) error {
		// Get the bucket which contains the payment, fail if the key
		// does not have a bucket.
		bucket := payments.NestedReadBucket(hash)
		if bucket == nil {
			return fmt.Errorf("key must be a bucket: '%v'",
				string(paymentsRootBucket))
		}

		paymentHashes = append(paymentHashes, string(hash))
		return nil
	}); err != nil {
		return err
	}

	for _, paymentHash := range paymentHashes {
		payment := payments.NestedReadWriteBucket([]byte(paymentHash))
		if payment.Get(paymentHtlcsBucket) != nil {
			return fmt.Errorf("key must be a bucket: '%v'",
				string(paymentHtlcsBucket))
		}

		htlcs := payment.NestedReadWriteBucket(paymentHtlcsBucket)
		if htlcs == nil {
			// Nothing to migrate for this payment.
			continue
		}

		if err := migrateHtlcsBucket(htlcs); err != nil {
			return err
		}
	}

	return nil
}

// migrateHtlcsBucket is a helper to gather, transform and re-store htlc attempt
// key/values.
func migrateHtlcsBucket(htlcs kvdb.RwBucket) error {
	// Collect attempt ids so that we can migrate attempts one-by-one
	// to avoid any bugs bbolt might have when invalidating cursors.
	var aids []string

	// First we collect all htlc attempt ids.
	if err := htlcs.ForEach(func(aid, v []byte) error {
		aids = append(aids, string(aid))
		return nil
	}); err != nil {
		return err
	}

	// Next we go over these attempts, fetch all data and migrate.
	for _, aid := range aids {
		aidKey := []byte(aid)
		attempt := htlcs.NestedReadWriteBucket(aidKey)
		if attempt == nil {
			return fmt.Errorf("non bucket element '%v' in '%v' "+
				"bucket", aidKey, string(paymentHtlcsBucket))
		}

		// Collect attempt/settle/fail infos.
		attemptInfo := attempt.Get(oldAttemptInfoKey)
		if len(attemptInfo) > 0 {
			newKey := htlcBucketKey(htlcAttemptInfoKey, aidKey)
			if err := htlcs.Put(newKey, attemptInfo); err != nil {
				return err
			}
		}

		settleInfo := attempt.Get(oldSettleInfoKey)
		if len(settleInfo) > 0 {
			newKey := htlcBucketKey(htlcSettleInfoKey, aidKey)
			if err := htlcs.Put(newKey, settleInfo); err != nil {
				return err
			}
		}

		failInfo := attempt.Get(oldFailInfoKey)
		if len(failInfo) > 0 {
			newKey := htlcBucketKey(htlcFailInfoKey, aidKey)
			if err := htlcs.Put(newKey, failInfo); err != nil {
				return err
			}
		}
	}

	// Finally we delete old attempt buckets.
	for _, aid := range aids {
		if err := htlcs.DeleteNestedBucket([]byte(aid)); err != nil {
			return err
		}
	}

	return nil
}
