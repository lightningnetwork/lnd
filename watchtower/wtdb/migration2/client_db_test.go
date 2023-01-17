package migration2

import (
	"encoding/binary"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (

	// pre is the expected data in the cChanSummaryBkt bucket before the
	// migration.
	pre = map[string]interface{}{
		channelIDString(1): string([]byte{1, 2, 3}),
		channelIDString(2): string([]byte{3, 4, 5}),
	}

	// pre should fail the migration due to no channel summary being found
	// for a given channel ID.
	preFailCorruptDB = map[string]interface{}{
		channelIDString(1): string([]byte{1, 2, 3}),
		channelIDString(2): "",
	}

	// post is the expected data after migration.
	post = map[string]interface{}{
		channelIDString(1): map[string]interface{}{
			string(cChannelSummary): string([]byte{1, 2, 3}),
		},
		channelIDString(2): map[string]interface{}{
			string(cChannelSummary): string([]byte{3, 4, 5}),
		},
	}
)

// TestMigrateClientChannelDetails tests that the MigrateClientChannelDetails
// function correctly moves channel-summaries from the channel-summaries bucket
// to the new channel-details bucket.
func TestMigrateClientChannelDetails(t *testing.T) {
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
			pre:        pre,
			post:       post,
		},
		{
			name:       "fail due to corrupt db",
			shouldFail: true,
			pre:        preFailCorruptDB,
			post:       nil,
		},
		{
			name:       "no channel summaries",
			shouldFail: false,
			pre:        nil,
			post:       nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Before the migration we have a channel summary
			// bucket.
			before := func(tx kvdb.RwTx) error {
				return migtest.RestoreDB(
					tx, cChanSummaryBkt, test.pre,
				)
			}

			// After the migration, we should have a new channel
			// details bucket and no longer have a channel summary
			// bucket.
			after := func(tx kvdb.RwTx) error {
				// If we expect our migration to fail, we
				// expect our channel summary bucket to remain
				// intact.
				if test.shouldFail {
					return migtest.VerifyDB(
						tx, cChanSummaryBkt, test.pre,
					)
				}

				// Otherwise, we expect the channel summary
				// bucket to be deleted.
				err := migtest.VerifyDB(
					tx, cChanSummaryBkt, test.pre,
				)
				require.ErrorContains(t, err, "not found")

				// We also expect the new channel details bucket
				// to be present.
				return migtest.VerifyDB(
					tx, cChanDetailsBkt, test.post,
				)
			}

			migtest.ApplyMigration(
				t, before, after, MigrateClientChannelDetails,
				test.shouldFail,
			)
		})
	}
}

func channelIDString(id uint64) string {
	var chanID ChannelID
	binary.BigEndian.PutUint64(chanID[:], id)
	return chanID.String()
}
