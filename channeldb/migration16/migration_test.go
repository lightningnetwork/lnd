package migration16

import (
	"encoding/hex"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	hexStr = migtest.Hex

	hash1Str   = "02acee76ebd53d00824410cf6adecad4f50334dac702bd5a2d3ba01b91709f0e"
	hash1      = hexStr(hash1Str)
	paymentID1 = hexStr("0000000000000001")

	hash2Str   = "62eb3f0a48f954e495d0c14ac63df04a67cefa59dafdbcd3d5046d1f5647840c"
	hash2      = hexStr(hash2Str)
	paymentID2 = hexStr("0000000000000002")

	paymentID3 = hexStr("0000000000000003")

	// pre is the data in the payments root bucket in database version 13 format.
	pre = map[string]interface{}{
		// A payment without duplicates.
		hash1: map[string]interface{}{
			"payment-sequence-key": paymentID1,
		},

		// A payment with a duplicate.
		hash2: map[string]interface{}{
			"payment-sequence-key": paymentID2,
			"payment-duplicate-bucket": map[string]interface{}{
				paymentID3: map[string]interface{}{
					"payment-sequence-key": paymentID3,
				},
			},
		},
	}

	preFails = map[string]interface{}{
		// A payment without duplicates.
		hash1: map[string]interface{}{
			"payment-sequence-key": paymentID1,
			"payment-duplicate-bucket": map[string]interface{}{
				paymentID1: map[string]interface{}{
					"payment-sequence-key": paymentID1,
				},
			},
		},
	}

	// post is the expected data after migration.
	post = map[string]interface{}{
		paymentID1: paymentHashIndex(hash1Str),
		paymentID2: paymentHashIndex(hash2Str),
		paymentID3: paymentHashIndex(hash2Str),
	}
)

// paymentHashIndex produces a string that represents the value we expect for
// our payment indexes from a hex encoded payment hash string.
func paymentHashIndex(hashStr string) string {
	hash, err := hex.DecodeString(hashStr)
	if err != nil {
		panic(err)
	}

	bytes, err := serializePaymentIndexEntry(hash)
	if err != nil {
		panic(err)
	}

	return string(bytes)
}

// MigrateSequenceIndex asserts that the database is properly migrated to
// contain a payments index.
func TestMigrateSequenceIndex(t *testing.T) {
	tests := []struct {
		name       string
		shouldFail bool
		pre        map[string]interface{}
		post       map[string]interface{}
	}{
		{
			name:       "migration ok",
			shouldFail: false,
			pre:        pre,
			post:       post,
		},
		{
			name:       "duplicate sequence number",
			shouldFail: true,
			pre:        preFails,
			post:       post,
		},
		{
			name:       "no payments",
			shouldFail: false,
			pre:        nil,
			post:       nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Before the migration we have a payments bucket.
			before := func(tx kvdb.RwTx) error {
				return migtest.RestoreDB(
					tx, paymentsRootBucket, test.pre,
				)
			}

			// After the migration, we should have an untouched
			// payments bucket and a new index bucket.
			after := func(tx kvdb.RwTx) error {
				if err := migtest.VerifyDB(
					tx, paymentsRootBucket, test.pre,
				); err != nil {
					return err
				}

				// If we expect our migration to fail, we don't
				// expect an index bucket.
				if test.shouldFail {
					return nil
				}

				return migtest.VerifyDB(
					tx, paymentsIndexBucket, test.post,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateSequenceIndex,
				test.shouldFail,
			)
		})
	}
}
