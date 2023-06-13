package migration7

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// preDetails is the expected data of the channel details bucket before
	// the migration.
	preDetails = map[string]interface{}{
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
		channelIDString(30): map[string]interface{}{},
	}

	// channelIDIndex is the data in the channelID index that is used to
	// find the mapping between the db-assigned channel ID and the real
	// channel ID.
	channelIDIndex = map[string]interface{}{
		uint64ToStr(10): channelIDString(100),
		uint64ToStr(20): channelIDString(222),
	}

	// sessions is the expected data in the sessions bucket before and
	// after the migration.
	sessions = map[string]interface{}{
		sessionIDString("1"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(10): map[string]interface{}{
					uint64ToStr(30): uint64ToStr(32),
					uint64ToStr(34): uint64ToStr(34),
				},
				uint64ToStr(20): map[string]interface{}{
					uint64ToStr(30): uint64ToStr(30),
				},
			},
			string(cSessionDBID): uint64ToStr(66),
		},
		sessionIDString("2"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(10): map[string]interface{}{
					uint64ToStr(33): uint64ToStr(33),
				},
			},
			string(cSessionDBID): uint64ToStr(77),
		},
	}

	// postDetails is the expected data in the channel details bucket after
	// the migration.
	postDetails = map[string]interface{}{
		channelIDString(100): map[string]interface{}{
			string(cChannelSummary): string([]byte{1, 2, 3}),
			string(cChanSessions): map[string]interface{}{
				uint64ToStr(66): string([]byte{1}),
				uint64ToStr(77): string([]byte{1}),
			},
		},
		channelIDString(222): map[string]interface{}{
			string(cChannelSummary): string([]byte{4, 5, 6}),
			string(cChanSessions): map[string]interface{}{
				uint64ToStr(66): string([]byte{1}),
			},
		},
	}
)

// TestMigrateChannelToSessionIndex tests that the MigrateChannelToSessionIndex
// function correctly builds the new channel-to-sessionID index to the tower
// client DB.
func TestMigrateChannelToSessionIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		shouldFail   bool
		preDetails   map[string]interface{}
		preSessions  map[string]interface{}
		preChanIndex map[string]interface{}
		postDetails  map[string]interface{}
	}{
		{
			name:         "migration ok",
			shouldFail:   false,
			preDetails:   preDetails,
			preSessions:  sessions,
			preChanIndex: channelIDIndex,
			postDetails:  postDetails,
		},
		{
			name:        "fail due to corrupt db",
			shouldFail:  true,
			preDetails:  preFailCorruptDB,
			preSessions: sessions,
		},
		{
			name:       "no sessions",
			shouldFail: false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Before the migration we have a channel details
			// bucket, a sessions bucket, a session ID index bucket
			// and a channel ID index bucket.
			before := func(tx kvdb.RwTx) error {
				err := migtest.RestoreDB(
					tx, cChanDetailsBkt, test.preDetails,
				)
				if err != nil {
					return err
				}

				err = migtest.RestoreDB(
					tx, cSessionBkt, test.preSessions,
				)
				if err != nil {
					return err
				}

				return migtest.RestoreDB(
					tx, cChanIDIndexBkt, test.preChanIndex,
				)
			}

			after := func(tx kvdb.RwTx) error {
				// If the migration fails, the details bucket
				// should be untouched.
				if test.shouldFail {
					return migtest.VerifyDB(
						tx, cChanDetailsBkt,
						test.preDetails,
					)
				}

				// Else, we expect an updated details bucket
				// and a new index bucket.
				return migtest.VerifyDB(
					tx, cChanDetailsBkt, test.postDetails,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateChannelToSessionIndex,
				test.shouldFail,
			)
		})
	}
}

func sessionIDString(id string) string {
	var sessID SessionID
	copy(sessID[:], id)
	return sessID.String()
}

func channelIDString(id uint64) string {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return string(chanID[:])
}

func uint64ToStr(id uint64) string {
	b, err := writeUint64(id)
	if err != nil {
		panic(err)
	}

	return string(b)
}
