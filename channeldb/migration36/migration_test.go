package migration36

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	hexStr = migtest.Hex

	outpoint1 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec58ce90fb463ad")
	outpoint2 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec58ce952d6c6c7")

	chanID1 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec5ef44")
	chanID2 = hexStr("81b637d8fcd2c6da6859e6963113a1170de793e4b725b84d1e0b4cf99ec54a2e")

	outpointData = map[string]interface{}{
		outpoint1: hexStr("000100"),
		outpoint2: hexStr("000101"),
	}

	chanIDBefore = map[string]interface{}{
		chanID1: "",
		chanID2: "",
	}

	chanIDAfter = map[string]interface{}{
		chanID1: outpoint1,
		chanID2: outpoint2,
	}
)

func TestMigrateChannelIDOutpointIndex(t *testing.T) {
	before := func(tx kvdb.RwTx) error {
		if err := migtest.RestoreDB(tx, outpointBucket, outpointData); err != nil {
			return err
		}
		return migtest.RestoreDB(tx, chanIDBucket, chanIDBefore)
	}

	after := func(tx kvdb.RwTx) error {
		if err := migtest.VerifyDB(tx, outpointBucket, outpointData); err != nil {
			return err
		}
		if err := migtest.VerifyDB(tx, chanIDBucket, chanIDAfter); err != nil {
			return err
		}

		// A second pass must preserve the same mapping.
		if err := MigrateChanIDOutpoints(tx); err != nil {
			return err
		}
		return migtest.VerifyDB(tx, chanIDBucket, chanIDAfter)
	}

	migtest.ApplyMigration(
		t, before, after, MigrateChanIDOutpoints, false,
	)
}

func TestMigrateChannelIDOutpointIndexRejectsMalformedKey(t *testing.T) {
	outpoints := map[string]interface{}{
		hexStr("010203"): hexStr("000100"),
	}

	before := func(tx kvdb.RwTx) error {
		if err := migtest.RestoreDB(tx, outpointBucket, outpoints); err != nil {
			return err
		}
		return migtest.RestoreDB(tx, chanIDBucket, map[string]interface{}{})
	}

	after := func(tx kvdb.RwTx) error {
		if err := migtest.VerifyDB(tx, outpointBucket, outpoints); err != nil {
			return err
		}
		return migtest.VerifyDB(tx, chanIDBucket, map[string]interface{}{})
	}

	migtest.ApplyMigration(
		t, before, after, MigrateChanIDOutpoints, true,
	)
}

func TestMigrateChannelIDOutpointIndexRejectsCollision(t *testing.T) {
	// Distinct outpoints can produce the same ChannelID because the funding
	// output index is XORed with the final two bytes of the transaction ID.
	firstHash := chainhash.Hash{}
	firstHash[0] = 1
	secondHash := firstHash
	secondHash[31] ^= 1

	firstPoint := wire.OutPoint{Hash: firstHash, Index: 0}
	secondPoint := wire.OutPoint{Hash: secondHash, Index: 1}

	firstID := lnwire.NewChanIDFromOutPoint(firstPoint)
	secondID := lnwire.NewChanIDFromOutPoint(secondPoint)
	require.Equal(t, firstID, secondID)

	var firstKey, secondKey bytes.Buffer
	require.NoError(t, graphdb.WriteOutpoint(&firstKey, &firstPoint))
	require.NoError(t, graphdb.WriteOutpoint(&secondKey, &secondPoint))
	require.Len(t, firstKey.Bytes(), 36)
	require.Len(t, secondKey.Bytes(), 36)

	outpoints := map[string]interface{}{
		string(firstKey.Bytes()):  hexStr("000100"),
		string(secondKey.Bytes()): hexStr("000101"),
	}

	before := func(tx kvdb.RwTx) error {
		if err := migtest.RestoreDB(tx, outpointBucket, outpoints); err != nil {
			return err
		}
		return migtest.RestoreDB(tx, chanIDBucket, map[string]interface{}{})
	}

	// The failed migration must roll back the first mapping as well.
	after := func(tx kvdb.RwTx) error {
		if err := migtest.VerifyDB(tx, outpointBucket, outpoints); err != nil {
			return err
		}
		return migtest.VerifyDB(tx, chanIDBucket, map[string]interface{}{})
	}

	migtest.ApplyMigration(
		t, before, after, MigrateChanIDOutpoints, true,
	)
}
