package migration27

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
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

	// dummyInput is used in our commit tx.
	dummyInput = &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0xffffffff,
		},
		Sequence: 0xffffffff,
	}

	// toLocalScript is the PkScript used in to-local output.
	toLocalScript = []byte{
		0x0, 0x14, 0xc6, 0x9, 0x62, 0xab, 0x60, 0xbe,
		0x40, 0xd, 0xab, 0x31, 0xc, 0x13, 0x14, 0x15,
		0x93, 0xe6, 0xa2, 0x94, 0xe4, 0x2a,
	}

	// commitTx1 is the tx saved in the first old revocation.
	commitTx1 = &wire.MsgTx{
		Version: 2,
		// Add a dummy input.
		TxIn: []*wire.TxIn{dummyInput},
		TxOut: []*wire.TxOut{
			{
				Value:    990_950,
				PkScript: toLocalScript,
			},
		},
	}

	// testChanStatus specifies the following channel status,
	// ChanStatusLocalDataLoss|ChanStatusRestored|ChanStatusRemoteCloseInitiator
	testChanStatus = mig25.ChannelStatus(0x4c)
)

// TestMigrateHistoricalBalances checks that the initial balances fields are
// patched to the historical channel info.
func TestMigrateHistoricalBalances(t *testing.T) {
	testCases := []struct {
		name               string
		isAfterMigration25 bool
		isRestored         bool
	}{
		{
			// Test that when the restored historical channel
			// doesn't have the two new fields.
			name:               "restored before migration25",
			isAfterMigration25: false,
			isRestored:         true,
		},
		{
			// Test that when the restored historical channel have
			// the two new fields.
			name:               "restored after migration25",
			isAfterMigration25: true,
			isRestored:         true,
		},
		{
			// Test that when the historical channel with a default
			// channel status flag doesn't have the two new fields.
			name:               "default before migration25",
			isAfterMigration25: false,
			isRestored:         false,
		},
		{
			// Test that when the historical channel with a default
			// channel status flag have the two new fields.
			name:               "default after migration25",
			isAfterMigration25: true,
			isRestored:         false,
		},
	}

	for _, tc := range testCases {
		tc := tc

		// testChannel is used to test the balance fields are correctly
		// set.
		testChannel := &OpenChannel{}
		testChannel.IdentityPub = dummyPubKey
		testChannel.FundingOutpoint = dummyOp
		testChannel.FundingTxn = commitTx1
		testChannel.IsInitiator = true

		// Set the channel status flag is we are testing the restored
		// case.
		if tc.isRestored {
			testChannel.ChanStatus = testChanStatus
		}

		// Create before and after migration functions.
		beforeFn := genBeforeMigration(
			testChannel, tc.isAfterMigration25,
		)
		afterFn := genAfterMigration(testChannel, tc.isRestored)

		// Run the test.
		t.Run(tc.name, func(t *testing.T) {
			migtest.ApplyMigration(
				t, beforeFn, afterFn,
				MigrateHistoricalBalances, false,
			)
		})
	}
}

func genBeforeMigration(c *OpenChannel, regression bool) func(kvdb.RwTx) error {
	return func(tx kvdb.RwTx) error {
		// Create the channel bucket.
		chanBucket, err := createHistoricalBucket(tx, c)
		if err != nil {
			return err
		}

		// Save the channel info using legacy format.
		if regression {
			// If test regression, then the historical channel
			// would have the two fields created. Thus we use the
			// method from migration26 which will save the two
			// fields for when legacy is true.
			return mig26.PutChanInfo(
				chanBucket, &c.OpenChannel, true,
			)
		}

		// Otherwise we will save the channel without the new fields.
		return PutChanInfo(chanBucket, c, true)
	}
}

func genAfterMigration(c *OpenChannel, restored bool) func(kvdb.RwTx) error {
	return func(tx kvdb.RwTx) error {
		chanBucket, err := fetchHistoricalChanBucket(tx, c)
		if err != nil {
			return err
		}

		newChan := &OpenChannel{}

		// Fetch the channel info using the new format.
		//
		// NOTE: this is the main testing point where we check the
		// deserialization of the historical channel bucket is correct.
		err = FetchChanInfo(chanBucket, newChan, false)
		if err != nil {
			return err
		}

		// Check our initial amount is correct.
		if newChan.InitialLocalBalance != 0 {
			return fmt.Errorf("wrong local balance, got %d, "+
				"want %d", newChan.InitialLocalBalance,
				c.InitialLocalBalance)
		}

		// Check their initial amount is correct.
		if newChan.InitialRemoteBalance != 0 {
			return fmt.Errorf("wrong remote balance, got %d, "+
				"want %d", newChan.InitialRemoteBalance,
				c.InitialRemoteBalance)
		}

		// We also check the relevant channel info fields stay the
		// same.
		if !newChan.IdentityPub.IsEqual(c.IdentityPub) {
			return fmt.Errorf("wrong IdentityPub")
		}
		if newChan.FundingOutpoint != c.FundingOutpoint {
			return fmt.Errorf("wrong FundingOutpoint")
		}
		if !newChan.IsInitiator {
			return fmt.Errorf("wrong IsInitiator")
		}

		// If it's restored, there should be no funding tx.
		if restored {
			if newChan.FundingTxn != nil {
				return fmt.Errorf("expect nil FundingTxn")
			}
			return nil
		}

		// Otherwise check the funding tx is read as expected.
		if newChan.FundingTxn.TxHash() != commitTx1.TxHash() {
			return fmt.Errorf("wrong FundingTxn")
		}

		return nil
	}
}

func createHistoricalBucket(tx kvdb.RwTx,
	c *OpenChannel) (kvdb.RwBucket, error) {

	// First fetch the top level bucket which stores all data related to
	// historical channels.
	rootBucket, err := tx.CreateTopLevelBucket(historicalChannelBucket)
	if err != nil {
		return nil, err
	}

	var chanPointBuf bytes.Buffer
	err = mig.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return nil, err
	}

	// Create the sub-bucket.
	return rootBucket.CreateBucketIfNotExists(chanPointBuf.Bytes())
}

func fetchHistoricalChanBucket(tx kvdb.RTx,
	c *OpenChannel) (kvdb.RBucket, error) {

	rootBucket := tx.ReadBucket(historicalChannelBucket)
	if rootBucket == nil {
		return nil, fmt.Errorf("expected a rootBucket")
	}

	var chanPointBuf bytes.Buffer
	err := mig.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return nil, err
	}

	return rootBucket.NestedReadBucket(chanPointBuf.Bytes()), nil
}
