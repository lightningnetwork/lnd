package migration3

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (

	// pre is the expected data in the cChanDetailsBkt bucket before the
	// migration.
	pre = map[string]interface{}{
		channelIDString(100): map[string]interface{}{
			string(cChannelSummary): string([]byte{1, 2, 3}),
		},
		channelIDString(222): map[string]interface{}{
			string(cChannelSummary): string([]byte{4, 5, 6}),
		},
	}

	// preFailCorruptDB should fail the migration due to no channel summary
	// being found for a given channel ID.
	preFailCorruptDB = map[string]interface{}{
		channelIDString(100): map[string]interface{}{},
	}

	// post is the expected data in the new index after migration.
	postIndex = map[string]interface{}{
		indexToString(1): channelIDString(100),
		indexToString(2): channelIDString(222),
	}

	// postDetails is the expected data in the cChanDetailsBkt bucket after
	// the migration.
	postDetails = map[string]interface{}{
		channelIDString(100): map[string]interface{}{
			string(cChannelSummary): string([]byte{1, 2, 3}),
			string(cChanDBID):       indexToString(1),
		},
		channelIDString(222): map[string]interface{}{
			string(cChannelSummary): string([]byte{4, 5, 6}),
			string(cChanDBID):       indexToString(2),
		},
	}
)

// TestMigrateChannelIDIndex tests that the MigrateChannelIDIndex function
// correctly adds a new channel-id index to the DB and also correctly updates
// the existing channel-details bucket.
func TestMigrateChannelIDIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		shouldFail  bool
		pre         map[string]interface{}
		postDetails map[string]interface{}
		postIndex   map[string]interface{}
	}{
		{
			name:        "migration ok",
			shouldFail:  false,
			pre:         pre,
			postDetails: postDetails,
			postIndex:   postIndex,
		},
		{
			name:       "fail due to corrupt db",
			shouldFail: true,
			pre:        preFailCorruptDB,
		},
		{
			name:        "no channel details",
			shouldFail:  false,
			pre:         nil,
			postDetails: nil,
			postIndex:   nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Before the migration we have a details bucket.
			before := func(tx kvdb.RwTx) error {
				return migtest.RestoreDB(
					tx, cChanDetailsBkt, test.pre,
				)
			}

			// After the migration, we should have an untouched
			// summary bucket and a new index bucket.
			after := func(tx kvdb.RwTx) error {
				// If the migration fails, the details bucket
				// should be untouched.
				if test.shouldFail {
					return migtest.VerifyDB(
						tx, cChanDetailsBkt, test.pre,
					)
				}

				// Else, we expect an updated summary bucket
				// and a new index bucket.
				err := migtest.VerifyDB(
					tx, cChanDetailsBkt, test.postDetails,
				)
				if err != nil {
					return err
				}

				return migtest.VerifyDB(
					tx, cChanIDIndexBkt, test.postIndex,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateChannelIDIndex,
				test.shouldFail,
			)
		})
	}
}

func indexToString(id uint64) string {
	var newIndex bytes.Buffer
	err := tlv.WriteVarInt(&newIndex, id, &[8]byte{})
	if err != nil {
		panic(err)
	}

	return newIndex.String()
}

func channelIDString(id uint64) string {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return chanID.String()
}
