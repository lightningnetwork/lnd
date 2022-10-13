package migration1

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	s1 = &ClientSessionBody{
		TowerID: TowerID(1),
	}
	s2 = &ClientSessionBody{
		TowerID: TowerID(3),
	}
	s3 = &ClientSessionBody{
		TowerID: TowerID(6),
	}

	// pre is the expected data in the DB before the migration.
	pre = map[string]interface{}{
		sessionIDString("1"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s1),
		},
		sessionIDString("2"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s3),
		},
		sessionIDString("3"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s1),
		},
		sessionIDString("4"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s1),
		},
		sessionIDString("5"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s2),
		},
	}

	// preFailNoSessionBody should fail the migration due to there being a
	// session without an associated session body.
	preFailNoSessionBody = map[string]interface{}{
		sessionIDString("1"): map[string]interface{}{
			string(cSessionBody): clientSessionString(s1),
		},
		sessionIDString("2"): map[string]interface{}{},
	}

	// post is the expected data after migration.
	post = map[string]interface{}{
		towerIDString(1): map[string]interface{}{
			sessionIDString("1"): string([]byte{1}),
			sessionIDString("3"): string([]byte{1}),
			sessionIDString("4"): string([]byte{1}),
		},
		towerIDString(3): map[string]interface{}{
			sessionIDString("5"): string([]byte{1}),
		},
		towerIDString(6): map[string]interface{}{
			sessionIDString("2"): string([]byte{1}),
		},
	}
)

// TestMigrateTowerToSessionIndex tests that the TestMigrateTowerToSessionIndex
// function correctly adds a new towerID-to-sessionID index to the tower client
// db.
func TestMigrateTowerToSessionIndex(t *testing.T) {
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
			name:       "fail due to corrupt db",
			shouldFail: true,
			pre:        preFailNoSessionBody,
			post:       nil,
		},
		{
			name:       "no sessions",
			shouldFail: false,
			pre:        nil,
			post:       nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Before the migration we have a sessions bucket.
			before := func(tx kvdb.RwTx) error {
				return migtest.RestoreDB(
					tx, cSessionBkt, test.pre,
				)
			}

			// After the migration, we should have an untouched
			// sessions bucket and a new index bucket.
			after := func(tx kvdb.RwTx) error {
				if err := migtest.VerifyDB(
					tx, cSessionBkt, test.pre,
				); err != nil {
					return err
				}

				// If we expect our migration to fail, we don't
				// expect an index bucket.
				if test.shouldFail {
					return nil
				}

				return migtest.VerifyDB(
					tx, cTowerIDToSessionIDIndexBkt,
					test.post,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateTowerToSessionIndex,
				test.shouldFail,
			)
		})
	}
}

func sessionIDString(id string) string {
	var sessID SessionID
	copy(sessID[:], id)
	return string(sessID[:])
}

func clientSessionString(s *ClientSessionBody) string {
	var b bytes.Buffer
	err := s.Encode(&b)
	if err != nil {
		panic(err)
	}

	return b.String()
}

func towerIDString(id int) string {
	towerID := TowerID(id)
	return string(towerID.Bytes())
}
