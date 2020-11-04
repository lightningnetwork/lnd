package migration19

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
)

var (
	hexStr = migtest.Hex

	varintOpString = hexStr("20166f95b104cf2a8d6c158de72ef0b78632f6a9e104ac4233867475714e8c5b1700000000")
	normalOpString = hexStr("166f95b104cf2a8d6c158de72ef0b78632f6a9e104ac4233867475714e8c5b1700000000")
	data           = hexStr("00010000000000000000")

	pre = map[string]interface{}{
		varintOpString: data,
	}

	post = map[string]interface{}{
		normalOpString: data,
	}
)

// TestMigrateOutpoint asserts that the channelOpeningState bucket
// is properly migrated.
func TestMigrateOutpoint(t *testing.T) {
	before := func(tx kvdb.RwTx) error {
		return migtest.RestoreDB(tx, channelOpeningStateBucket, pre)

	}

	after := func(tx kvdb.RwTx) error {
		return migtest.VerifyDB(tx, channelOpeningStateBucket, post)
	}

	migtest.ApplyMigration(t, before, after, MigrateOutpoint, false)
}
