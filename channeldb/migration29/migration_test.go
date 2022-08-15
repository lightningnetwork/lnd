package migration29

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	hexStr = migtest.Hex

	outpoint1 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec58ce90fb463ad")
	outpoint2 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec58ce952d6c6c7")

	chanID1 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec5ef44")
	chanID2 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec54a2e")

	// These tlv streams are used to populate the outpoint bucket at the
	// start of the test.
	tlvOutpointOpen   = hexStr("000100")
	tlvOutpointClosed = hexStr("000101")

	// outpointData is used to populate the outpoint bucket.
	outpointData = map[string]interface{}{
		outpoint1: tlvOutpointOpen,
		outpoint2: tlvOutpointClosed,
	}

	// chanIDBefore is the ChannelID bucket before the migration.
	chanIDBefore = map[string]interface{}{}

	// post is the expected data in the ChannelID bucket after the
	// migration.
	post = map[string]interface{}{
		chanID1: "",
		chanID2: "",
	}
)

// TestMigrateChannelIDIndex asserts that the ChannelID index is properly
// populated.
func TestMigrateChannelIDIndex(t *testing.T) {
	// Prime the database with the populated outpoint bucket. We create the
	// ChannelID bucket since the prior migration creates it anyways.
	before := func(tx kvdb.RwTx) error {
		err := migtest.RestoreDB(tx, outpointBucket, outpointData)
		if err != nil {
			return err
		}

		return migtest.RestoreDB(tx, chanIDBucket, chanIDBefore)
	}

	// After the migration, ensure that the ChannelID bucket is properly
	// populated.
	after := func(tx kvdb.RwTx) error {
		err := migtest.VerifyDB(tx, outpointBucket, outpointData)
		if err != nil {
			return err
		}

		return migtest.VerifyDB(tx, chanIDBucket, post)
	}

	migtest.ApplyMigration(t, before, after, MigrateChanID, false)
}
