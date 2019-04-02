package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestPaymentStatusesMigration checks that already completed payments will have
// their payment statuses set to Completed after the migration.
func TestPaymentStatusesMigration(t *testing.T) {
	t.Parallel()

	fakePayment := makeFakePayment()
	paymentHash := sha256.Sum256(fakePayment.PaymentPreimage[:])

	// Add fake payment to test database, verifying that it was created,
	// that we have only one payment, and its status is not "Completed".
	beforeMigrationFunc := func(d *DB) {
		if err := d.AddPayment(fakePayment); err != nil {
			t.Fatalf("unable to add payment: %v", err)
		}

		payments, err := d.FetchAllPayments()
		if err != nil {
			t.Fatalf("unable to fetch payments: %v", err)
		}

		if len(payments) != 1 {
			t.Fatalf("wrong qty of paymets: expected 1, got %v",
				len(payments))
		}

		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		// We should receive default status if we have any in database.
		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}

		// Lastly, we'll add a locally-sourced circuit and
		// non-locally-sourced circuit to the circuit map. The
		// locally-sourced payment should end up with an InFlight
		// status, while the other should remain unchanged, which
		// defaults to Grounded.
		err = d.Update(func(tx *bbolt.Tx) error {
			circuits, err := tx.CreateBucketIfNotExists(
				[]byte("circuit-adds"),
			)
			if err != nil {
				return err
			}

			groundedKey := make([]byte, 16)
			binary.BigEndian.PutUint64(groundedKey[:8], 1)
			binary.BigEndian.PutUint64(groundedKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is the case for locally-sourced
			// payments. No payment status should end up being set
			// for this circuit, since the short channel id of the
			// key is non-zero (e.g., a forwarded circuit). This
			// will default it to Grounded.
			groundedCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			err = circuits.Put(groundedKey, groundedCircuit)
			if err != nil {
				return err
			}

			inFlightKey := make([]byte, 16)
			binary.BigEndian.PutUint64(inFlightKey[:8], 0)
			binary.BigEndian.PutUint64(inFlightKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is not the case for forwarded
			// payments, but should have no impact on the
			// correctness of the test. The payment status for this
			// circuit should be set to InFlight, since the short
			// channel id in the key is 0 (sourceHop).
			inFlightCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			return circuits.Put(inFlightKey, inFlightCircuit)
		})
		if err != nil {
			t.Fatalf("unable to add circuit map entry: %v", err)
		}
	}

	// Verify that the created payment status is "Completed" for our one
	// fake payment.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta(nil)
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 1 {
			t.Fatal("migration 'paymentStatusesMigration' wasn't applied")
		}

		// Check that our completed payments were migrated.
		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusCompleted {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusCompleted.String(), paymentStatus.String())
		}

		inFlightHash := [32]byte{
			0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that the locally sourced payment was transitioned to
		// InFlight.
		paymentStatus, err = d.FetchPaymentStatus(inFlightHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusInFlight {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusInFlight.String(), paymentStatus.String())
		}

		groundedHash := [32]byte{
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that non-locally sourced payments remain in the default
		// Grounded state.
		paymentStatus, err = d.FetchPaymentStatus(groundedHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		paymentStatusesMigration,
		false)
}

// TestMigrateOptionalChannelCloseSummaryFields properly converts a
// ChannelCloseSummary to the v7 format, where optional fields have their
// presence indicated with boolean markers.
func TestMigrateOptionalChannelCloseSummaryFields(t *testing.T) {
	t.Parallel()

	chanState, err := createTestChannelState(nil)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	var chanPointBuf bytes.Buffer
	err = writeOutpoint(&chanPointBuf, &chanState.FundingOutpoint)
	if err != nil {
		t.Fatalf("unable to write outpoint: %v", err)
	}

	chanID := chanPointBuf.Bytes()

	testCases := []struct {
		closeSummary     *ChannelCloseSummary
		oldSerialization func(c *ChannelCloseSummary) []byte
	}{
		{
			// A close summary where none of the new fields are
			// set.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:      chanState.FundingOutpoint,
				ShortChanID:    chanState.ShortChanID(),
				ChainHash:      chanState.ChainHash,
				ClosingTXID:    testTx.TxHash(),
				CloseHeight:    100,
				RemotePub:      chanState.IdentityPub,
				Capacity:       chanState.Capacity,
				SettledBalance: btcutil.Amount(50000),
				CloseType:      RemoteForceClose,
				IsPending:      true,

				// The last fields will be unset.
				RemoteCurrentRevocation: nil,
				LocalChanConfig:         ChannelConfig{},
				RemoteNextRevocation:    nil,
			},

			// In the old format the last field written is the
			// IsPendingField. It should be converted by adding an
			// extra boolean marker at the end to indicate that the
			// remaining fields are not there.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				// For the old format, these are all the fields
				// that are written.
				return buf.Bytes()
			},
		},
		{
			// A close summary where the new fields are present,
			// but the optional RemoteNextRevocation field is not
			// set.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:               chanState.FundingOutpoint,
				ShortChanID:             chanState.ShortChanID(),
				ChainHash:               chanState.ChainHash,
				ClosingTXID:             testTx.TxHash(),
				CloseHeight:             100,
				RemotePub:               chanState.IdentityPub,
				Capacity:                chanState.Capacity,
				SettledBalance:          btcutil.Amount(50000),
				CloseType:               RemoteForceClose,
				IsPending:               true,
				RemoteCurrentRevocation: chanState.RemoteCurrentRevocation,
				LocalChanConfig:         chanState.LocalChanCfg,

				// RemoteNextRevocation is optional, and here
				// it is not set.
				RemoteNextRevocation: nil,
			},

			// In the old format the last field written is the
			// LocalChanConfig. This indicates that the optional
			// RemoteNextRevocation field is not present. It should
			// be converted by adding boolean markers for all these
			// fields.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteCurrentRevocation)
				if err != nil {
					t.Fatal(err)
				}

				err = writeChanConfig(&buf, &cs.LocalChanConfig)
				if err != nil {
					t.Fatal(err)
				}

				// RemoteNextRevocation is not written.
				return buf.Bytes()
			},
		},
		{
			// A close summary where all fields are present.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:               chanState.FundingOutpoint,
				ShortChanID:             chanState.ShortChanID(),
				ChainHash:               chanState.ChainHash,
				ClosingTXID:             testTx.TxHash(),
				CloseHeight:             100,
				RemotePub:               chanState.IdentityPub,
				Capacity:                chanState.Capacity,
				SettledBalance:          btcutil.Amount(50000),
				CloseType:               RemoteForceClose,
				IsPending:               true,
				RemoteCurrentRevocation: chanState.RemoteCurrentRevocation,
				LocalChanConfig:         chanState.LocalChanCfg,

				// RemoteNextRevocation is optional, and in
				// this case we set it.
				RemoteNextRevocation: chanState.RemoteNextRevocation,
			},

			// In the old format all the fields are written. It
			// should be converted by adding boolean markers for
			// all these fields.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteCurrentRevocation)
				if err != nil {
					t.Fatal(err)
				}

				err = writeChanConfig(&buf, &cs.LocalChanConfig)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteNextRevocation)
				if err != nil {
					t.Fatal(err)
				}

				return buf.Bytes()
			},
		},
	}

	for _, test := range testCases {

		// Before the migration we must add the old format to the DB.
		beforeMigrationFunc := func(d *DB) {

			// Get the old serialization format for this test's
			// close summary, and it to the closed channel bucket.
			old := test.oldSerialization(test.closeSummary)
			err = d.Update(func(tx *bbolt.Tx) error {
				closedChanBucket, err := tx.CreateBucketIfNotExists(
					closedChannelBucket,
				)
				if err != nil {
					return err
				}
				return closedChanBucket.Put(chanID, old)
			})
			if err != nil {
				t.Fatalf("unable to add old serialization: %v",
					err)
			}
		}

		// After the migration it should be found in the new format.
		afterMigrationFunc := func(d *DB) {
			meta, err := d.FetchMeta(nil)
			if err != nil {
				t.Fatal(err)
			}

			if meta.DbVersionNumber != 1 {
				t.Fatal("migration wasn't applied")
			}

			// We generate the new serialized version, to check
			// against what is found in the DB.
			var b bytes.Buffer
			err = serializeChannelCloseSummary(&b, test.closeSummary)
			if err != nil {
				t.Fatalf("unable to serialize: %v", err)
			}
			newSerialization := b.Bytes()

			var dbSummary []byte
			err = d.View(func(tx *bbolt.Tx) error {
				closedChanBucket := tx.Bucket(closedChannelBucket)
				if closedChanBucket == nil {
					return errors.New("unable to find bucket")
				}

				// Get the serialized verision from the DB and
				// make sure it matches what we expected.
				dbSummary = closedChanBucket.Get(chanID)
				if !bytes.Equal(dbSummary, newSerialization) {
					return fmt.Errorf("unexpected new " +
						"serialization")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("unable to view DB: %v", err)
			}

			// Finally we fetch the deserialized summary from the
			// DB and check that it is equal to our original one.
			dbChannels, err := d.FetchClosedChannels(false)
			if err != nil {
				t.Fatalf("unable to fetch closed channels: %v",
					err)
			}

			if len(dbChannels) != 1 {
				t.Fatalf("expected 1 closed channels, found %v",
					len(dbChannels))
			}

			dbChan := dbChannels[0]
			if !reflect.DeepEqual(dbChan, test.closeSummary) {
				dbChan.RemotePub.Curve = nil
				test.closeSummary.RemotePub.Curve = nil
				t.Fatalf("not equal: %v vs %v",
					spew.Sdump(dbChan),
					spew.Sdump(test.closeSummary))
			}

		}

		applyMigration(t,
			beforeMigrationFunc,
			afterMigrationFunc,
			migrateOptionalChannelCloseSummaryFields,
			false)
	}
}

// TestMigrateGossipMessageStoreKeys ensures that the migration to the new
// gossip message store key format is successful/unsuccessful under various
// scenarios.
func TestMigrateGossipMessageStoreKeys(t *testing.T) {
	t.Parallel()

	// Construct the message which we'll use to test the migration, along
	// with its old and new key formats.
	shortChanID := lnwire.ShortChannelID{BlockHeight: 10}
	msg := &lnwire.AnnounceSignatures{ShortChannelID: shortChanID}

	var oldMsgKey [33 + 8]byte
	copy(oldMsgKey[:33], pubKey.SerializeCompressed())
	binary.BigEndian.PutUint64(oldMsgKey[33:41], shortChanID.ToUint64())

	var newMsgKey [33 + 8 + 2]byte
	copy(newMsgKey[:41], oldMsgKey[:])
	binary.BigEndian.PutUint16(newMsgKey[41:43], uint16(msg.MsgType()))

	// Before the migration, we'll create the bucket where the messages
	// should live and insert them.
	beforeMigration := func(db *DB) {
		var b bytes.Buffer
		if err := msg.Encode(&b, 0); err != nil {
			t.Fatalf("unable to serialize message: %v", err)
		}

		err := db.Update(func(tx *bbolt.Tx) error {
			messageStore, err := tx.CreateBucketIfNotExists(
				messageStoreBucket,
			)
			if err != nil {
				return err
			}

			return messageStore.Put(oldMsgKey[:], b.Bytes())
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// After the migration, we'll make sure that:
	//   1. We cannot find the message under its old key.
	//   2. We can find the message under its new key.
	//   3. The message matches the original.
	afterMigration := func(db *DB) {
		meta, err := db.FetchMeta(nil)
		if err != nil {
			t.Fatalf("unable to fetch db version: %v", err)
		}
		if meta.DbVersionNumber != 1 {
			t.Fatalf("migration should have succeeded but didn't")
		}

		var rawMsg []byte
		err = db.View(func(tx *bbolt.Tx) error {
			messageStore := tx.Bucket(messageStoreBucket)
			if messageStore == nil {
				return errors.New("message store bucket not " +
					"found")
			}
			rawMsg = messageStore.Get(oldMsgKey[:])
			if rawMsg != nil {
				t.Fatal("expected to not find message under " +
					"old key, but did")
			}
			rawMsg = messageStore.Get(newMsgKey[:])
			if rawMsg == nil {
				return fmt.Errorf("expected to find message " +
					"under new key, but didn't")
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		gotMsg, err := lnwire.ReadMessage(bytes.NewReader(rawMsg), 0)
		if err != nil {
			t.Fatalf("unable to deserialize raw message: %v", err)
		}
		if !reflect.DeepEqual(msg, gotMsg) {
			t.Fatalf("expected message: %v\ngot message: %v",
				spew.Sdump(msg), spew.Sdump(gotMsg))
		}
	}

	applyMigration(
		t, beforeMigration, afterMigration,
		migrateGossipMessageStoreKeys, false,
	)
}
