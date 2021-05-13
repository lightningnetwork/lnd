package migration_01_to_11

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/kvdb"
)

// MigrateRouteSerialization migrates the way we serialize routes across the
// entire database. At the time of writing of this migration, this includes our
// payment attempts, as well as the payment results in mission control.
func MigrateRouteSerialization(tx kvdb.RwTx) error {
	// First, we'll do all the payment attempts.
	rootPaymentBucket := tx.ReadWriteBucket(paymentsRootBucket)
	if rootPaymentBucket == nil {
		return nil
	}

	// As we can't mutate a bucket while we're iterating over it with
	// ForEach, we'll need to collect all the known payment hashes in
	// memory first.
	var payHashes [][]byte
	err := rootPaymentBucket.ForEach(func(k, v []byte) error {
		if v != nil {
			return nil
		}

		payHashes = append(payHashes, k)
		return nil
	})
	if err != nil {
		return err
	}

	// Now that we have all the payment hashes, we can carry out the
	// migration itself.
	for _, payHash := range payHashes {
		payHashBucket := rootPaymentBucket.NestedReadWriteBucket(payHash)

		// First, we'll migrate the main (non duplicate) payment to
		// this hash.
		err := migrateAttemptEncoding(tx, payHashBucket)
		if err != nil {
			return err
		}

		// Now that we've migrated the main payment, we'll also check
		// for any duplicate payments to the same payment hash.
		dupBucket := payHashBucket.NestedReadWriteBucket(paymentDuplicateBucket)

		// If there's no dup bucket, then we can move on to the next
		// payment.
		if dupBucket == nil {
			continue
		}

		// Otherwise, we'll now iterate through all the duplicate pay
		// hashes and migrate those.
		var dupSeqNos [][]byte
		err = dupBucket.ForEach(func(k, v []byte) error {
			dupSeqNos = append(dupSeqNos, k)
			return nil
		})
		if err != nil {
			return err
		}

		// Now in this second pass, we'll re-serialize their duplicate
		// payment attempts under the new encoding.
		for _, seqNo := range dupSeqNos {
			dupPayHashBucket := dupBucket.NestedReadWriteBucket(seqNo)
			err := migrateAttemptEncoding(tx, dupPayHashBucket)
			if err != nil {
				return err
			}
		}
	}

	log.Infof("Migration of route/hop serialization complete!")

	log.Infof("Migrating to new mission control store by clearing " +
		"existing data")

	resultsKey := []byte("missioncontrol-results")
	err = tx.DeleteTopLevelBucket(resultsKey)
	if err != nil && err != kvdb.ErrBucketNotFound {
		return err
	}

	log.Infof("Migration to new mission control completed!")

	return nil
}

// migrateAttemptEncoding migrates payment attempts using the legacy format to
// the new format.
func migrateAttemptEncoding(tx kvdb.RwTx, payHashBucket kvdb.RwBucket) error {
	payAttemptBytes := payHashBucket.Get(paymentAttemptInfoKey)
	if payAttemptBytes == nil {
		return nil
	}

	// For our migration, we'll first read out the existing payment attempt
	// using the legacy serialization of the attempt.
	payAttemptReader := bytes.NewReader(payAttemptBytes)
	payAttempt, err := deserializePaymentAttemptInfoLegacy(
		payAttemptReader,
	)
	if err != nil {
		return err
	}

	// Now that we have the old attempts, we'll explicitly mark this as
	// needing a legacy payload, since after this migration, the modern
	// payload will be the default if signalled.
	for _, hop := range payAttempt.Route.Hops {
		hop.LegacyPayload = true
	}

	// Finally, we'll write out the payment attempt using the new encoding.
	var b bytes.Buffer
	err = serializePaymentAttemptInfo(&b, payAttempt)
	if err != nil {
		return err
	}

	return payHashBucket.Put(paymentAttemptInfoKey, b.Bytes())
}

func deserializePaymentAttemptInfoLegacy(r io.Reader) (*PaymentAttemptInfo, error) {
	a := &PaymentAttemptInfo{}
	err := ReadElements(r, &a.PaymentID, &a.SessionKey)
	if err != nil {
		return nil, err
	}
	a.Route, err = deserializeRouteLegacy(r)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func serializePaymentAttemptInfoLegacy(w io.Writer, a *PaymentAttemptInfo) error {
	if err := WriteElements(w, a.PaymentID, a.SessionKey); err != nil {
		return err
	}

	if err := serializeRouteLegacy(w, a.Route); err != nil {
		return err
	}

	return nil
}

func deserializeHopLegacy(r io.Reader) (*Hop, error) {
	h := &Hop{}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(h.PubKeyBytes[:], pub)

	if err := ReadElements(r,
		&h.ChannelID, &h.OutgoingTimeLock, &h.AmtToForward,
	); err != nil {
		return nil, err
	}

	return h, nil
}

func serializeHopLegacy(w io.Writer, h *Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:], h.ChannelID, h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	return nil
}

func deserializeRouteLegacy(r io.Reader) (Route, error) {
	rt := Route{}
	if err := ReadElements(r,
		&rt.TotalTimeLock, &rt.TotalAmount,
	); err != nil {
		return rt, err
	}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return rt, err
	}
	copy(rt.SourcePubKey[:], pub)

	var numHops uint32
	if err := ReadElements(r, &numHops); err != nil {
		return rt, err
	}

	var hops []*Hop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHopLegacy(r)
		if err != nil {
			return rt, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	return rt, nil
}

func serializeRouteLegacy(w io.Writer, r Route) error {
	if err := WriteElements(w,
		r.TotalTimeLock, r.TotalAmount, r.SourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHopLegacy(w, h); err != nil {
			return err
		}
	}

	return nil
}
