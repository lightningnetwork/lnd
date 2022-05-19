package migration26

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// Create dummy values to be stored in db.
	dummyPrivKey, _ = btcec.NewPrivateKey()
	dummyPubKey     = dummyPrivKey.PubKey()
	dummyOp         = wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 9,
	}

	// ourAmt and theirAmt are the initial balances found in the local
	// channel commitment at height 0.
	testOurAmt   = lnwire.MilliSatoshi(500_000)
	testTheirAmt = lnwire.MilliSatoshi(1000_000)

	// testChannel is used to test the balance fields are correctly set.
	testChannel = &OpenChannel{
		OpenChannel: mig25.OpenChannel{
			OpenChannel: mig.OpenChannel{
				IdentityPub:     dummyPubKey,
				FundingOutpoint: dummyOp,
			},
		},
	}
)

// TestMigrateBalancesToTlvRecords checks that the initial balances fields are
// saved using the tlv records.
func TestMigrateBalancesToTlvRecords(t *testing.T) {
	testCases := []struct {
		name                string
		ourAmt              lnwire.MilliSatoshi
		theirAmt            lnwire.MilliSatoshi
		beforeMigrationFunc func(kvdb.RwTx) error
		afterMigrationFunc  func(kvdb.RwTx) error
		shouldFail          bool
	}{
		{
			// Test when both balance fields are non-zero.
			name:                "non-zero local and remote",
			ourAmt:              testOurAmt,
			theirAmt:            testTheirAmt,
			beforeMigrationFunc: genBeforeMigration(testChannel),
			afterMigrationFunc:  genAfterMigration(testChannel),
		},
		{
			// Test when local balance is non-zero.
			name:                "non-zero local balance",
			ourAmt:              testOurAmt,
			theirAmt:            0,
			beforeMigrationFunc: genBeforeMigration(testChannel),
			afterMigrationFunc:  genAfterMigration(testChannel),
		},
		{
			// Test when remote balance is non-zero.
			name:                "non-zero remote balance",
			ourAmt:              0,
			theirAmt:            testTheirAmt,
			beforeMigrationFunc: genBeforeMigration(testChannel),
			afterMigrationFunc:  genAfterMigration(testChannel),
		},
		{
			// Test when both balance fields are zero.
			name:                "zero local and remote",
			ourAmt:              0,
			theirAmt:            0,
			beforeMigrationFunc: genBeforeMigration(testChannel),
			afterMigrationFunc:  genAfterMigration(testChannel),
		},
	}

	for _, tc := range testCases {
		tc := tc

		// Before running the test, set the balance fields based on the
		// test params.
		testChannel.InitialLocalBalance = tc.ourAmt
		testChannel.InitialRemoteBalance = tc.theirAmt

		t.Run(tc.name, func(t *testing.T) {
			migtest.ApplyMigration(
				t,
				tc.beforeMigrationFunc,
				tc.afterMigrationFunc,
				MigrateBalancesToTlvRecords,
				tc.shouldFail,
			)
		})
	}
}

func genBeforeMigration(c *OpenChannel) func(kvdb.RwTx) error {
	return func(tx kvdb.RwTx) error {
		// Create the channel bucket.
		chanBucket, err := mig25.CreateChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}

		// Save the channel info using legacy format.
		if err := PutChanInfo(chanBucket, c, true); err != nil {
			return err
		}

		return nil
	}
}

func genAfterMigration(c *OpenChannel) func(kvdb.RwTx) error {
	return func(tx kvdb.RwTx) error {
		chanBucket, err := mig25.FetchChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}

		newChan := &OpenChannel{}

		// Fetch the channel info using the new format.
		err = FetchChanInfo(chanBucket, newChan, false)
		if err != nil {
			return err
		}

		// Check our initial amount is correct.
		if newChan.InitialLocalBalance != c.InitialLocalBalance {
			return fmt.Errorf("wrong local balance, got %d, "+
				"want %d", newChan.InitialLocalBalance,
				c.InitialLocalBalance)
		}

		// Check their initial amount is correct.
		if newChan.InitialRemoteBalance != c.InitialRemoteBalance {
			return fmt.Errorf("wrong remote balance, got %d, "+
				"want %d", newChan.InitialRemoteBalance,
				c.InitialRemoteBalance)
		}

		// We also check the relevant channel info fields stay the
		// same.
		if !newChan.IdentityPub.IsEqual(dummyPubKey) {
			return fmt.Errorf("wrong IdentityPub")
		}
		if newChan.FundingOutpoint != dummyOp {
			return fmt.Errorf("wrong FundingOutpoint")
		}

		return nil
	}
}
