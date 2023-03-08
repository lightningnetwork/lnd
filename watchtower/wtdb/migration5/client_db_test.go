package migration5

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// preIndex is the data in the tower-to-session index before the
	// migration.
	preIndex = map[string]interface{}{
		towerIDString(1): map[string]interface{}{
			sessionIDString("1"): string([]byte{1}),
			sessionIDString("3"): string([]byte{1}),
		},
		towerIDString(3): map[string]interface{}{
			sessionIDString("4"): string([]byte{1}),
		},
	}

	// preIndexBadTowerID has an invalid TowerID. This is used to test that
	// the migration correctly rolls back on failure.
	preIndexBadTowerID = map[string]interface{}{
		"1": map[string]interface{}{},
	}

	// towerDBEmpty is the data in an empty tower bucket before the
	// migration.
	towerDBEmpty = map[string]interface{}{}

	towerDBMatchIndex = map[string]interface{}{
		towerIDString(1): map[string]interface{}{},
		towerIDString(3): map[string]interface{}{},
	}

	towerDBWithExtraEntries = map[string]interface{}{
		towerIDString(1): map[string]interface{}{},
		towerIDString(3): map[string]interface{}{},
		towerIDString(4): map[string]interface{}{},
	}

	// post is the expected data after migration.
	postIndex = map[string]interface{}{
		towerIDString(1): map[string]interface{}{
			sessionIDString("1"): string([]byte{1}),
			sessionIDString("3"): string([]byte{1}),
		},
		towerIDString(3): map[string]interface{}{
			sessionIDString("4"): string([]byte{1}),
		},
		towerIDString(4): map[string]interface{}{},
	}
)

// TestCompleteTowerToSessionIndex tests that the
// MigrateCompleteTowerToSessionIndex function correctly completes the
// towerID-to-sessionID index in the tower client db.
func TestCompleteTowerToSessionIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shouldFail bool
		towerDB    map[string]interface{}
		pre        map[string]interface{}
		post       map[string]interface{}
	}{
		{
			name:    "no changes - empty tower db",
			towerDB: towerDBEmpty,
			pre:     preIndex,
			post:    preIndex,
		},
		{
			name:    "no changes - tower db matches index",
			towerDB: towerDBMatchIndex,
			pre:     preIndex,
			post:    preIndex,
		},
		{
			name:    "fill in missing towers",
			towerDB: towerDBWithExtraEntries,
			pre:     preIndex,
			post:    postIndex,
		},
		{
			name:       "fail due to corrupt db",
			shouldFail: true,
			towerDB:    preIndexBadTowerID,
			pre:        preIndex,
			post:       preIndex,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Before the migration we have a tower bucket and an
			// initial tower-to-session index bucket.
			before := func(tx kvdb.RwTx) error {
				err := migtest.RestoreDB(
					tx, cTowerBkt, test.towerDB,
				)
				if err != nil {
					return err
				}

				return migtest.RestoreDB(
					tx, cTowerIDToSessionIDIndexBkt,
					test.pre,
				)
			}

			// After the migration, we should have an untouched
			// tower bucket and a possibly tweaked tower-to-session
			// index bucket.
			after := func(tx kvdb.RwTx) error {
				if err := migtest.VerifyDB(
					tx, cTowerBkt, test.towerDB,
				); err != nil {
					return err
				}

				// If we expect our migration to fail, we don't
				// expect our index bucket to be unchanged.
				if test.shouldFail {
					return migtest.VerifyDB(
						tx, cTowerIDToSessionIDIndexBkt,
						test.pre,
					)
				}

				return migtest.VerifyDB(
					tx, cTowerIDToSessionIDIndexBkt,
					test.post,
				)
			}

			migtest.ApplyMigration(
				t, before, after,
				MigrateCompleteTowerToSessionIndex,
				test.shouldFail,
			)
		})
	}
}

func towerIDString(id int) string {
	towerID := TowerID(id)
	return string(towerID.Bytes())
}

func sessionIDString(id string) string {
	var sessID SessionID
	copy(sessID[:], id)
	return string(sessID[:])
}
