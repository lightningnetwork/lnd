package migration4

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// details is the expected data of the channel details bucket. This
	// bucket should not be changed during the migration, but it is used
	// to find the db-assigned ID for each channel.
	details = map[string]interface{}{
		channelIDString(1): map[string]interface{}{
			string(cChanDBID): uint64ToStr(10),
		},
		channelIDString(2): map[string]interface{}{
			string(cChanDBID): uint64ToStr(20),
		},
	}

	// preSessions is the expected data in the sessions bucket before the
	// migration.
	preSessions = map[string]interface{}{
		sessionIDString("1"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 30,
				}),
				"2": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 31,
				}),
				"3": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 32,
				}),
				"4": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 34,
				}),
				"5": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 30,
				}),
			},
		},
		sessionIDString("2"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 33,
				}),
			},
		},
		sessionIDString("3"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 35,
				}),
				"2": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 36,
				}),
				"3": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 28,
				}),
				"4": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 29,
				}),
			},
		},
	}

	// preMidStateDB is a possible state that the db could be in if the
	// migration started but was interrupted before completing. In this
	// state, some sessions still have the old cSessionAcks bucket along
	// with the new cSessionAckRangeIndex. This is a valid pre-state and
	// a migration on this state should succeed.
	preMidStateDB = map[string]interface{}{
		sessionIDString("1"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 30,
				}),
				"2": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 31,
				}),
				"3": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 32,
				}),
				"4": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 34,
				}),
				"5": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 30,
				}),
			},
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(10): map[string]interface{}{
					uint64ToStr(30): uint64ToStr(32),
					uint64ToStr(34): uint64ToStr(34),
				},
				uint64ToStr(20): map[string]interface{}{
					uint64ToStr(30): uint64ToStr(30),
				},
			},
		},
		sessionIDString("2"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 33,
				}),
			},
		},
		sessionIDString("3"): map[string]interface{}{
			string(cSessionAcks): map[string]interface{}{
				"1": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 35,
				}),
				"2": backupIDToString(&BackupID{
					ChanID:       intToChannelID(1),
					CommitHeight: 36,
				}),
				"3": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 28,
				}),
				"4": backupIDToString(&BackupID{
					ChanID:       intToChannelID(2),
					CommitHeight: 29,
				}),
			},
		},
	}

	// preFailCorruptDB should fail the migration due to no session data
	// being found for a given session ID.
	preFailCorruptDB = map[string]interface{}{
		sessionIDString("2"): "",
	}

	// postSessions is the expected data in the sessions bucket after the
	// migration.
	postSessions = map[string]interface{}{
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
		},
		sessionIDString("2"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(10): map[string]interface{}{
					uint64ToStr(33): uint64ToStr(33),
				},
			},
		},
		sessionIDString("3"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(10): map[string]interface{}{
					uint64ToStr(35): uint64ToStr(36),
				},
				uint64ToStr(20): map[string]interface{}{
					uint64ToStr(28): uint64ToStr(29),
				},
			},
		},
	}
)

// TestMigrateAckedUpdates tests that the MigrateAckedUpdates function correctly
// migrates the existing AckedUpdates bucket for each session to the new
// RangeIndex representation.
func TestMigrateAckedUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shouldFail bool
		pre        map[string]interface{}
		post       map[string]interface{}
	}{
		{
			name:       "migration ok",
			shouldFail: false,
			pre:        preSessions,
			post:       postSessions,
		},
		{
			name:       "migration ok after re-starting",
			shouldFail: false,
			pre:        preMidStateDB,
			post:       postSessions,
		},
		{
			name:       "fail due to corrupt db",
			shouldFail: true,
			pre:        preFailCorruptDB,
		},
		{
			name:       "no sessions details",
			shouldFail: false,
			pre:        nil,
			post:       nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			before := before(test.pre)

			// After the migration, we should have an untouched
			// summary bucket and a new index bucket.
			after := after(test.shouldFail, test.pre, test.post)

			migtest.ApplyMigrationWithDB(
				t, before, after, MigrateAckedUpdates(2),
				test.shouldFail,
			)
		})
	}
}

// before returns a call-back function that can be used to set up a db's
// cChanDetailsBkt along with the cSessionBkt using the passed preMigDB
// structure.
func before(preMigDB map[string]interface{}) func(backend kvdb.Backend) error {
	return func(db kvdb.Backend) error {
		return db.Update(func(tx walletdb.ReadWriteTx) error {
			err := migtest.RestoreDB(
				tx, cChanDetailsBkt, details,
			)
			if err != nil {
				return err
			}

			return migtest.RestoreDB(
				tx, cSessionBkt, preMigDB,
			)
		}, func() {})
	}
}

// after returns a call-back function that can be used to verify the state of
// a db post migration.
func after(shouldFail bool, preMigDB,
	postMigDB map[string]interface{}) func(backend kvdb.Backend) error {

	return func(db kvdb.Backend) error {
		return db.Update(func(tx walletdb.ReadWriteTx) error {
			// The channel details bucket should remain untouched.
			err := migtest.VerifyDB(tx, cChanDetailsBkt, details)
			if err != nil {
				return err
			}

			// If the migration fails, the sessions bucket should be
			// untouched.
			if shouldFail {
				return migtest.VerifyDB(
					tx, cSessionBkt, preMigDB,
				)
			}

			return migtest.VerifyDB(tx, cSessionBkt, postMigDB)
		}, func() {})
	}
}

func sessionIDString(id string) string {
	var sessID SessionID
	copy(sessID[:], id)
	return sessID.String()
}

func intToChannelID(id uint64) ChannelID {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return chanID
}

func channelIDString(id uint64) string {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return string(chanID[:])
}

func uint64ToStr(id uint64) string {
	b, err := writeBigSize(id)
	if err != nil {
		panic(err)
	}

	return string(b)
}

func backupIDToString(backup *BackupID) string {
	var b bytes.Buffer
	_ = backup.Encode(&b)
	return b.String()
}
