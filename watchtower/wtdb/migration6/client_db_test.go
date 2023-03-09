package migration6

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// pre is the expected data in the sessions bucket before the migration.
	pre = map[string]interface{}{
		sessionIDToString(100): map[string]interface{}{
			string(cSessionBody): string([]byte{1, 2, 3}),
		},
		sessionIDToString(222): map[string]interface{}{
			string(cSessionBody): string([]byte{4, 5, 6}),
		},
	}

	// preFailCorruptDB should fail the migration due to no session body
	// being found for a given session ID.
	preFailCorruptDB = map[string]interface{}{
		sessionIDToString(100): "",
	}

	// post is the expected session index after migration.
	postIndex = map[string]interface{}{
		indexToString(1): sessionIDToString(100),
		indexToString(2): sessionIDToString(222),
	}

	// postSessions is the expected data in the sessions bucket after the
	// migration.
	postSessions = map[string]interface{}{
		sessionIDToString(100): map[string]interface{}{
			string(cSessionBody): string([]byte{1, 2, 3}),
			string(cSessionDBID): indexToString(1),
		},
		sessionIDToString(222): map[string]interface{}{
			string(cSessionBody): string([]byte{4, 5, 6}),
			string(cSessionDBID): indexToString(2),
		},
	}
)

// TestMigrateSessionIDIndex tests that the MigrateSessionIDIndex function
// correctly adds a new session-id index to the DB and also correctly updates
// the existing session bucket.
func TestMigrateSessionIDIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		shouldFail   bool
		pre          map[string]interface{}
		postSessions map[string]interface{}
		postIndex    map[string]interface{}
	}{
		{
			name:         "migration ok",
			shouldFail:   false,
			pre:          pre,
			postSessions: postSessions,
			postIndex:    postIndex,
		},
		{
			name:       "fail due to corrupt db",
			shouldFail: true,
			pre:        preFailCorruptDB,
		},
		{
			name:         "no channel details",
			shouldFail:   false,
			pre:          nil,
			postSessions: nil,
			postIndex:    nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Before the migration we have a details bucket.
			before := func(tx kvdb.RwTx) error {
				return migtest.RestoreDB(
					tx, cSessionBkt, test.pre,
				)
			}

			// After the migration, we should have an untouched
			// summary bucket and a new index bucket.
			after := func(tx kvdb.RwTx) error {
				// If the migration fails, the details bucket
				// should be untouched.
				if test.shouldFail {
					if err := migtest.VerifyDB(
						tx, cSessionBkt, test.pre,
					); err != nil {
						return err
					}

					return nil
				}

				// Else, we expect an updated summary bucket
				// and a new index bucket.
				err := migtest.VerifyDB(
					tx, cSessionBkt, test.postSessions,
				)
				if err != nil {
					return err
				}

				return migtest.VerifyDB(
					tx, cSessionIDIndexBkt, test.postIndex,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateSessionIDIndex,
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

func sessionIDToString(id uint64) string {
	var chanID SessionID
	byteOrder.PutUint64(chanID[:], id)
	return chanID.String()
}
