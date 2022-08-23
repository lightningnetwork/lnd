package migration25

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig24 "github.com/lightningnetwork/lnd/channeldb/migration24"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// Create dummy values to be stored in db.
	dummyPrivKey, _ = btcec.NewPrivateKey()
	dummyPubKey     = dummyPrivKey.PubKey()
	dummySig        = []byte{1, 2, 3}
	dummyTx         = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				Sequence: 0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
			},
		},
		LockTime: 5,
	}

	dummyOp = wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 9,
	}
	dummyHTLC = mig.HTLC{
		Signature:     dummySig,
		RHash:         [32]byte{},
		Amt:           100_000,
		RefundTimeout: 731583,
		OutputIndex:   1,
		Incoming:      true,
		HtlcIndex:     1,
		LogIndex:      1,
	}

	// ourAmt and theirAmt are the initial balances found in the local
	// channel commitment at height 0.
	ourAmt   = lnwire.MilliSatoshi(500_000)
	theirAmt = lnwire.MilliSatoshi(1000_000)

	// ourAmtRevoke and theirAmtRevoke are the initial balances found in
	// the revocation log at height 0.
	//
	// NOTE: they are made differently such that we can easily check the
	// source when patching the balances.
	ourAmtRevoke   = lnwire.MilliSatoshi(501_000)
	theirAmtRevoke = lnwire.MilliSatoshi(1001_000)

	// remoteCommit0 is the channel commitment at commit height 0. This is
	// also the revocation log we will use to patch the initial balances.
	remoteCommit0 = mig.ChannelCommitment{
		LocalBalance:  ourAmtRevoke,
		RemoteBalance: theirAmtRevoke,
		CommitFee:     btcutil.Amount(1),
		FeePerKw:      btcutil.Amount(1),
		CommitTx:      dummyTx,
		CommitSig:     dummySig,
		Htlcs:         []mig.HTLC{},
	}

	// localCommit0 is the channel commitment at commit height 0. This is
	// the channel commitment we will use to patch the initial balances
	// when there are no revocation logs.
	localCommit0 = mig.ChannelCommitment{
		LocalBalance:  ourAmt,
		RemoteBalance: theirAmt,
		CommitFee:     btcutil.Amount(1),
		FeePerKw:      btcutil.Amount(1),
		CommitTx:      dummyTx,
		CommitSig:     dummySig,
		Htlcs:         []mig.HTLC{},
	}

	// remoteCommit1 and localCommit1 are the channel commitment at commit
	// height 1.
	remoteCommit1 = mig.ChannelCommitment{
		CommitHeight:    1,
		LocalLogIndex:   1,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  1,
		RemoteHtlcIndex: 1,
		LocalBalance:    ourAmt - dummyHTLC.Amt,
		RemoteBalance:   theirAmt + dummyHTLC.Amt,
		CommitFee:       btcutil.Amount(1),
		FeePerKw:        btcutil.Amount(1),
		CommitTx:        dummyTx,
		CommitSig:       dummySig,
		Htlcs:           []mig.HTLC{dummyHTLC},
	}
	localCommit1 = mig.ChannelCommitment{
		CommitHeight:    1,
		LocalLogIndex:   1,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  1,
		RemoteHtlcIndex: 1,
		LocalBalance:    ourAmt - dummyHTLC.Amt,
		RemoteBalance:   theirAmt + dummyHTLC.Amt,
		CommitFee:       btcutil.Amount(1),
		FeePerKw:        btcutil.Amount(1),
		CommitTx:        dummyTx,
		CommitSig:       dummySig,
		Htlcs:           []mig.HTLC{dummyHTLC},
	}

	// openChannel0 is the OpenChannel at commit height 0. When this
	// variable is used, we expect to patch the initial balances from its
	// commitments.
	openChannel0 = &OpenChannel{
		OpenChannel: mig.OpenChannel{
			IdentityPub:      dummyPubKey,
			FundingOutpoint:  dummyOp,
			LocalCommitment:  localCommit0,
			RemoteCommitment: remoteCommit0,
		},
	}

	// openChannel1 is the OpenChannel at commit height 1. When this
	// variable is used, we expect to patch the initial balances from the
	// remote commitment at height 0.
	openChannel1 = &OpenChannel{
		OpenChannel: mig.OpenChannel{
			IdentityPub:      dummyPubKey,
			FundingOutpoint:  dummyOp,
			LocalCommitment:  localCommit1,
			RemoteCommitment: remoteCommit1,
		},
	}
)

// TestMigrateInitialBalances checks that the proper initial balances are
// patched to the channel info.
func TestMigrateInitialBalances(t *testing.T) {
	testCases := []struct {
		name                string
		beforeMigrationFunc func(kvdb.RwTx) error
		afterMigrationFunc  func(kvdb.RwTx) error
		shouldFail          bool
	}{
		{
			// Test that we patch the initial balances using the
			// revocation log.
			name: "patch balance from revoke log",
			beforeMigrationFunc: genBeforeMigration(
				openChannel1, &remoteCommit0,
			),
			afterMigrationFunc: genAfterMigration(
				ourAmtRevoke, theirAmtRevoke, openChannel1,
			),
		},
		{
			// Test that we patch the initial balances using the
			// channel's local commitment since at height 0,
			// balances found in LocalCommitment reflect the
			// initial balances.
			name: "patch balance from local commit",
			beforeMigrationFunc: genBeforeMigration(
				openChannel0, nil,
			),
			afterMigrationFunc: genAfterMigration(
				ourAmt, theirAmt, openChannel0,
			),
		},
		{
			// Test that we patch the initial balances using the
			// channel's local commitment even when there is a
			// revocation log available.
			name: "patch balance from local commit only",
			beforeMigrationFunc: genBeforeMigration(
				openChannel0, &remoteCommit0,
			),
			afterMigrationFunc: genAfterMigration(
				ourAmt, theirAmt, openChannel0,
			),
		},
		{
			// Test that when there is no revocation log the
			// migration would fail.
			name: "patch balance error on no revoke log",
			beforeMigrationFunc: genBeforeMigration(
				// Use nil to specify no revocation log will be
				// created.
				openChannel1, nil,
			),
			afterMigrationFunc: genAfterMigration(
				// Use nil to specify skipping the
				// afterMigrationFunc.
				0, 0, nil,
			),
			shouldFail: true,
		},
		{
			// Test that when the saved revocation log is not what
			// we want the migration would fail.
			name: "patch balance error on wrong revoke log",
			beforeMigrationFunc: genBeforeMigration(
				// Use the revocation log with the wrong
				// height.
				openChannel1, &remoteCommit1,
			),
			afterMigrationFunc: genAfterMigration(
				// Use nil to specify skipping the
				// afterMigrationFunc.
				0, 0, nil,
			),
			shouldFail: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			migtest.ApplyMigration(
				t,
				tc.beforeMigrationFunc,
				tc.afterMigrationFunc,
				MigrateInitialBalances,
				tc.shouldFail,
			)
		})
	}
}

func genBeforeMigration(c *OpenChannel,
	commit *mig.ChannelCommitment) func(kvdb.RwTx) error {

	return func(tx kvdb.RwTx) error {
		if c.InitialLocalBalance != 0 {
			return fmt.Errorf("non zero initial local amount")
		}

		if c.InitialRemoteBalance != 0 {
			return fmt.Errorf("non zero initial local amount")
		}

		// Create the channel bucket.
		chanBucket, err := CreateChanBucket(tx, c)
		if err != nil {
			return err
		}

		// Save the channel info using legacy format.
		if err := putChanInfo(chanBucket, c, true); err != nil {
			return err
		}

		// Save the channel commitments.
		if err := PutChanCommitments(chanBucket, c); err != nil {
			return err
		}

		// If we have a remote commitment, save it as our revocation
		// log.
		if commit != nil {
			err := putChannelLogEntryLegacy(chanBucket, commit)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func genAfterMigration(ourAmt, theirAmt lnwire.MilliSatoshi,
	c *OpenChannel) func(kvdb.RwTx) error {

	return func(tx kvdb.RwTx) error {
		// If the passed OpenChannel is nil, we will skip the
		// afterMigrationFunc as it indicates an error is expected
		// during the migration.
		if c == nil {
			return nil
		}

		chanBucket, err := FetchChanBucket(tx, c)
		if err != nil {
			return err
		}

		newChan := &OpenChannel{}

		// Fetch the channel info using the new format.
		err = fetchChanInfo(chanBucket, newChan, false)
		if err != nil {
			return err
		}

		// Check our initial amount is correct.
		if newChan.InitialLocalBalance != ourAmt {
			return fmt.Errorf("wrong local balance, got %d, "+
				"want %d", newChan.InitialLocalBalance, ourAmt)
		}

		// Check their initial amount is correct.
		if newChan.InitialRemoteBalance != theirAmt {
			return fmt.Errorf("wrong remote balance, got %d, "+
				"want %d", newChan.InitialRemoteBalance,
				theirAmt)
		}

		// We also check the relevant channel info fields stay the
		// same.
		if !newChan.IdentityPub.IsEqual(c.IdentityPub) {
			return fmt.Errorf("wrong IdentityPub")
		}
		if newChan.FundingOutpoint != c.FundingOutpoint {
			return fmt.Errorf("wrong FundingOutpoint")
		}

		return nil
	}
}

// putChannelLogEntryLegacy saves an old format revocation log to the bucket.
func putChannelLogEntryLegacy(chanBucket kvdb.RwBucket,
	commit *mig.ChannelCommitment) error {

	logBucket, err := chanBucket.CreateBucketIfNotExists(
		revocationLogBucketLegacy,
	)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := mig.SerializeChanCommit(&b, commit); err != nil {
		return err
	}

	logEntrykey := mig24.MakeLogKey(commit.CommitHeight)
	return logBucket.Put(logEntrykey[:], b.Bytes())
}
