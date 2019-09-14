package channeldb

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

func makeRandPaymentCreationInfo() (*PaymentCreationInfo, error) {
	var payHash lntypes.Hash
	if _, err := rand.Read(payHash[:]); err != nil {
		return nil, err
	}

	return &PaymentCreationInfo{
		PaymentHash:    payHash,
		Value:          lnwire.MilliSatoshi(rand.Int63()),
		CreationDate:   time.Now(),
		PaymentRequest: []byte("test"),
	}, nil
}

// TestPaymentRouteSerialization tests that we're able to properly migrate
// existing payments on disk that contain the traversed routes to the new
// routing format which supports the TLV payloads. We also test that the
// migration is able to handle duplicate payment attempts.
func TestPaymentRouteSerialization(t *testing.T) {
	t.Parallel()

	legacyHop1 := &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		LegacyPayload:    true,
		AmtToForward:     555,
	}
	legacyHop2 := &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		LegacyPayload:    true,
		AmtToForward:     555,
	}
	legacyRoute := route.Route{
		TotalTimeLock: 123,
		TotalAmount:   1234567,
		SourcePubKey:  route.NewVertex(pub),
		Hops:          []*route.Hop{legacyHop1, legacyHop2},
	}

	const numPayments = 4
	var oldPayments []*Payment

	sharedPayAttempt := PaymentAttemptInfo{
		PaymentID:  1,
		SessionKey: priv,
		Route:      legacyRoute,
	}

	// We'll first add a series of fake payments, using the existing legacy
	// serialization format.
	beforeMigrationFunc := func(d *DB) {
		err := d.Update(func(tx *bbolt.Tx) error {
			paymentsBucket, err := tx.CreateBucket(
				paymentsRootBucket,
			)
			if err != nil {
				t.Fatalf("unable to create new payments "+
					"bucket: %v", err)
			}

			for i := 0; i < numPayments; i++ {
				var seqNum [8]byte
				byteOrder.PutUint64(seqNum[:], uint64(i))

				// All payments will be randomly generated,
				// other than the final payment. We'll force
				// the final payment to re-use an existing
				// payment hash so we can insert it into the
				// duplicate payment hash bucket.
				var payInfo *PaymentCreationInfo
				if i < numPayments-1 {
					payInfo, err = makeRandPaymentCreationInfo()
					if err != nil {
						t.Fatalf("unable to create "+
							"payment: %v", err)
					}
				} else {
					payInfo = oldPayments[0].Info
				}

				// Next, legacy encoded when needed, we'll
				// serialize the info and the attempt.
				var payInfoBytes bytes.Buffer
				err = serializePaymentCreationInfo(
					&payInfoBytes, payInfo,
				)
				if err != nil {
					t.Fatalf("unable to encode pay "+
						"info: %v", err)
				}
				var payAttemptBytes bytes.Buffer
				err = serializePaymentAttemptInfoLegacy(
					&payAttemptBytes, &sharedPayAttempt,
				)
				if err != nil {
					t.Fatalf("unable to encode payment attempt: "+
						"%v", err)
				}

				// Before we write to disk, we'll need to fetch
				// the proper bucket. If this is the duplicate
				// payment, then we'll grab the dup bucket,
				// otherwise, we'll use the top level bucket.
				var payHashBucket *bbolt.Bucket
				if i < numPayments-1 {
					payHashBucket, err = paymentsBucket.CreateBucket(
						payInfo.PaymentHash[:],
					)
					if err != nil {
						t.Fatalf("unable to create payments bucket: %v", err)
					}
				} else {
					payHashBucket = paymentsBucket.Bucket(
						payInfo.PaymentHash[:],
					)
					dupPayBucket, err := payHashBucket.CreateBucket(
						paymentDuplicateBucket,
					)
					if err != nil {
						t.Fatalf("unable to create "+
							"dup hash bucket: %v", err)
					}

					payHashBucket, err = dupPayBucket.CreateBucket(
						seqNum[:],
					)
					if err != nil {
						t.Fatalf("unable to make dup "+
							"bucket: %v", err)
					}
				}

				err = payHashBucket.Put(paymentSequenceKey, seqNum[:])
				if err != nil {
					t.Fatalf("unable to write seqno: %v", err)
				}

				err = payHashBucket.Put(
					paymentCreationInfoKey, payInfoBytes.Bytes(),
				)
				if err != nil {
					t.Fatalf("unable to write creation "+
						"info: %v", err)
				}

				err = payHashBucket.Put(
					paymentAttemptInfoKey, payAttemptBytes.Bytes(),
				)
				if err != nil {
					t.Fatalf("unable to write attempt "+
						"info: %v", err)
				}

				oldPayments = append(oldPayments, &Payment{
					Info:    payInfo,
					Attempt: &sharedPayAttempt,
				})
			}

			return nil
		})
		if err != nil {
			t.Fatalf("unable to create test payments: %v", err)
		}
	}

	afterMigrationFunc := func(d *DB) {
		newPayments, err := d.FetchPayments()
		if err != nil {
			t.Fatalf("unable to fetch new payments: %v", err)
		}

		if len(newPayments) != numPayments {
			t.Fatalf("expected %d payments, got %d", numPayments,
				len(newPayments))
		}

		for i, p := range newPayments {
			// Order of payments should be be preserved.
			old := oldPayments[i]

			if p.Attempt.PaymentID != old.Attempt.PaymentID {
				t.Fatalf("wrong pay ID: expected %v, got %v",
					p.Attempt.PaymentID,
					old.Attempt.PaymentID)
			}

			if p.Attempt.Route.TotalFees() != old.Attempt.Route.TotalFees() {
				t.Fatalf("Fee mismatch")
			}

			if p.Attempt.Route.TotalAmount != old.Attempt.Route.TotalAmount {
				t.Fatalf("Total amount mismatch")
			}

			if p.Attempt.Route.TotalTimeLock != old.Attempt.Route.TotalTimeLock {
				t.Fatalf("timelock mismatch")
			}

			if p.Attempt.Route.SourcePubKey != old.Attempt.Route.SourcePubKey {
				t.Fatalf("source mismatch: %x vs %x",
					p.Attempt.Route.SourcePubKey[:],
					old.Attempt.Route.SourcePubKey[:])
			}

			for i, hop := range p.Attempt.Route.Hops {
				if !reflect.DeepEqual(hop, legacyRoute.Hops[i]) {
					t.Fatalf("hop mismatch")
				}
			}
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		migrateRouteSerialization,
		false)
}
