package migration30

import (
	"bytes"
	"fmt"
	"math/rand"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/shachain"
)

var (
	testChainHash = chainhash.Hash{1, 2, 3}
	testChanType  = mig25.SingleFunderTweaklessBit

	// testOurIndex and testTheirIndex are artificial indexes that're saved
	// to db during test setup. They are different from indexes populated
	// from the actual migration process so we can check whether a new
	// revocation log is overwritten or not.
	testOurIndex   = uint32(100)
	testTheirIndex = uint32(200)

	// dummyInput is used in our commit tx.
	dummyInput = &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0xffffffff,
		},
		Sequence: 0xffffffff,
	}

	// htlcScript is the PkScript used in the HTLC output. This script
	// corresponds to revocation preimage2.
	htlcScript = []byte{
		0x0, 0x20, 0x3d, 0x51, 0x66, 0xda, 0x39, 0x93,
		0x7b, 0x49, 0xaf, 0x2, 0xf2, 0x2f, 0x90, 0x52,
		0x8e, 0x45, 0x24, 0x34, 0x8f, 0xd8, 0x76, 0x7,
		0x5a, 0xfc, 0x52, 0x8d, 0x68, 0xdd, 0xbc, 0xce,
		0x3e, 0x5d,
	}

	// toLocalScript is the PkScript used in to-local output.
	toLocalScript = []byte{
		0x0, 0x14, 0xc6, 0x9, 0x62, 0xab, 0x60, 0xbe,
		0x40, 0xd, 0xab, 0x31, 0xc, 0x13, 0x14, 0x15,
		0x93, 0xe6, 0xa2, 0x94, 0xe4, 0x2a,
	}

	// preimage1 defines a revocation preimage, generated from itest.
	preimage1 = []byte{
		0x95, 0xb4, 0x7c, 0x5a, 0x2b, 0xfd, 0x6f, 0xf4,
		0x70, 0x8, 0xc, 0x70, 0x82, 0x36, 0xc8, 0x5,
		0x88, 0x16, 0xaf, 0x29, 0xb5, 0x8, 0xfd, 0x5a,
		0x40, 0x28, 0x24, 0xc, 0x2a, 0x7f, 0x96, 0xcd,
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

	// logHeight1 is the CommitHeight used by oldLog1.
	logHeight1 = uint64(0)

	// localBalance1 is the LocalBalance used in oldLog1.
	localBalance1 = lnwire.MilliSatoshi(990_950_000)

	// remoteBalance1 is the RemoteBalance used in oldLog1.
	remoteBalance1 = lnwire.MilliSatoshi(0)

	// oldLog1 defines an old revocation that has no HTLCs.
	oldLog1 = mig.ChannelCommitment{
		CommitHeight:    logHeight1,
		LocalLogIndex:   0,
		LocalHtlcIndex:  0,
		RemoteLogIndex:  0,
		RemoteHtlcIndex: 0,
		LocalBalance:    localBalance1,
		RemoteBalance:   remoteBalance1,
		CommitTx:        commitTx1,
	}

	// newLog1 is the new version of oldLog1 in the case were we don't want
	// to store any balance data.
	newLog1 = RevocationLog{
		OurOutputIndex:   0,
		TheirOutputIndex: OutputIndexEmpty,
		CommitTxHash:     commitTx1.TxHash(),
	}

	// newLog1WithAmts is the new version of oldLog1 in the case were we do
	// want to store balance data.
	newLog1WithAmts = RevocationLog{
		OurOutputIndex:   0,
		TheirOutputIndex: OutputIndexEmpty,
		CommitTxHash:     commitTx1.TxHash(),
		OurBalance:       &localBalance1,
		TheirBalance:     &remoteBalance1,
	}

	// preimage2 defines the second revocation preimage used in the test,
	// generated from itest.
	preimage2 = []byte{
		0xac, 0x60, 0x7a, 0x59, 0x9, 0xd6, 0x11, 0xb2,
		0xf5, 0x6e, 0xaa, 0xc6, 0xb9, 0x0, 0x12, 0xdc,
		0xf0, 0x89, 0x58, 0x90, 0x8a, 0xa2, 0xc6, 0xfc,
		0xf1, 0x2, 0x74, 0x87, 0x30, 0x51, 0x5e, 0xea,
	}

	// commitTx2 is the tx saved in the second old revocation.
	commitTx2 = &wire.MsgTx{
		Version: 2,
		// Add a dummy input.
		TxIn: []*wire.TxIn{dummyInput},
		TxOut: []*wire.TxOut{
			{
				Value:    100_000,
				PkScript: htlcScript,
			},
			{
				Value:    888_800,
				PkScript: toLocalScript,
			},
		},
	}

	// rHash is the payment hash used in the htlc below.
	rHash = [32]byte{
		0x42, 0x5e, 0xd4, 0xe4, 0xa3, 0x6b, 0x30, 0xea,
		0x21, 0xb9, 0xe, 0x21, 0xc7, 0x12, 0xc6, 0x49,
		0xe8, 0x21, 0x4c, 0x29, 0xb7, 0xea, 0xf6, 0x80,
		0x89, 0xd1, 0x3, 0x9c, 0x6e, 0x55, 0x38, 0x4c,
	}

	// htlc defines an HTLC that's saved in the old revocation log.
	htlc = mig.HTLC{
		RHash:         rHash,
		Amt:           lnwire.MilliSatoshi(100_000_000),
		RefundTimeout: 489,
		OutputIndex:   0,
		Incoming:      false,
		OnionBlob:     bytes.Repeat([]byte{0xff}, 1366),
		HtlcIndex:     0,
		LogIndex:      0,
	}

	// logHeight2 is the CommitHeight used by oldLog2.
	logHeight2 = uint64(1)

	// localBalance2 is the LocalBalance used in oldLog2.
	localBalance2 = lnwire.MilliSatoshi(888_800_000)

	// remoteBalance2 is the RemoteBalance used in oldLog2.
	remoteBalance2 = lnwire.MilliSatoshi(0)

	// oldLog2 defines an old revocation that has one HTLC.
	oldLog2 = mig.ChannelCommitment{
		CommitHeight:    logHeight2,
		LocalLogIndex:   1,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  0,
		RemoteHtlcIndex: 0,
		LocalBalance:    localBalance2,
		RemoteBalance:   remoteBalance2,
		CommitTx:        commitTx2,
		Htlcs:           []mig.HTLC{htlc},
	}

	// newLog2 is the new version of the oldLog2 in the case were we don't
	// want to store any balance data.
	newLog2 = RevocationLog{
		OurOutputIndex:   1,
		TheirOutputIndex: OutputIndexEmpty,
		CommitTxHash:     commitTx2.TxHash(),
		HTLCEntries: []*HTLCEntry{
			{
				RHash:         rHash,
				RefundTimeout: 489,
				OutputIndex:   0,
				Incoming:      false,
				Amt:           btcutil.Amount(100_000),
			},
		},
	}

	// newLog2WithAmts is the new version of oldLog2 in the case were we do
	// want to store balance data.
	newLog2WithAmts = RevocationLog{
		OurOutputIndex:   1,
		TheirOutputIndex: OutputIndexEmpty,
		CommitTxHash:     commitTx2.TxHash(),
		OurBalance:       &localBalance2,
		TheirBalance:     &remoteBalance2,
		HTLCEntries: []*HTLCEntry{
			{
				RHash:         rHash,
				RefundTimeout: 489,
				OutputIndex:   0,
				Incoming:      false,
				Amt:           btcutil.Amount(100_000),
			},
		},
	}

	// newLog3 defines an revocation log that's been created after v0.15.0.
	newLog3 = mig.ChannelCommitment{
		CommitHeight:    logHeight2 + 1,
		LocalLogIndex:   1,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  0,
		RemoteHtlcIndex: 0,
		LocalBalance:    lnwire.MilliSatoshi(888_800_000),
		RemoteBalance:   0,
		CommitTx:        commitTx2,
		Htlcs:           []mig.HTLC{htlc},
	}

	// The following public keys are taken from the itest results.
	localMusigKey, _ = btcec.ParsePubKey([]byte{
		0x2,
		0xda, 0x42, 0xa4, 0x4a, 0x6b, 0x42, 0xfe, 0xcb,
		0x2f, 0x7e, 0x35, 0x89, 0x99, 0xdd, 0x43, 0xba,
		0x4b, 0xf1, 0x9c, 0xf, 0x18, 0xef, 0x9, 0x83,
		0x35, 0x31, 0x59, 0xa4, 0x3b, 0xde, 0xa, 0xde,
	})
	localRevocationBasePoint, _ = btcec.ParsePubKey([]byte{
		0x2,
		0x6, 0x16, 0xd1, 0xb1, 0x4f, 0xee, 0x11, 0x86,
		0x55, 0xfe, 0x31, 0x66, 0x6f, 0x43, 0x1, 0x80,
		0xa8, 0xa7, 0x5c, 0x2, 0x92, 0xe5, 0x7c, 0x4,
		0x31, 0xa6, 0xcf, 0x43, 0xb6, 0xdb, 0xe6, 0x10,
	})
	localPaymentBasePoint, _ = btcec.ParsePubKey([]byte{
		0x2,
		0x88, 0x65, 0x16, 0xc2, 0x37, 0x3f, 0xc5, 0x16,
		0x62, 0x71, 0x0, 0xdd, 0x4d, 0x43, 0x28, 0x43,
		0x32, 0x91, 0x75, 0xcc, 0xd8, 0x81, 0xb6, 0xb0,
		0xd8, 0x96, 0x78, 0xad, 0x18, 0x3b, 0x16, 0xe1,
	})
	localDelayBasePoint, _ = btcec.ParsePubKey([]byte{
		0x2,
		0xea, 0x41, 0x48, 0x11, 0x2, 0x59, 0xe3, 0x5c,
		0x51, 0x15, 0x90, 0x25, 0x4a, 0x61, 0x5, 0x51,
		0xb3, 0x8, 0xe9, 0xd5, 0xf, 0xc6, 0x91, 0x25,
		0x14, 0xd2, 0xcf, 0xc8, 0xc5, 0x5b, 0xd9, 0x88,
	})
	localHtlcBasePoint, _ = btcec.ParsePubKey([]byte{
		0x3,
		0xfa, 0x1f, 0x6, 0x3a, 0xa4, 0x75, 0x2e, 0x74,
		0x3e, 0x55, 0x9, 0x20, 0x6e, 0xf6, 0xa8, 0xe1,
		0xd7, 0x61, 0x50, 0x75, 0xa8, 0x34, 0x15, 0xc3,
		0x6b, 0xdc, 0xb0, 0xbf, 0xaa, 0x66, 0xd7, 0xa7,
	})

	remoteMultiSigKey, _ = btcec.ParsePubKey([]byte{
		0x2,
		0x2b, 0x88, 0x7c, 0x6a, 0xf8, 0xb3, 0x51, 0x61,
		0xd3, 0x1c, 0xf1, 0xe4, 0x43, 0xc2, 0x8c, 0x5e,
		0xfa, 0x8e, 0xb5, 0xe9, 0xd0, 0x14, 0xb5, 0x33,
		0x6a, 0xcc, 0xd, 0x11, 0x42, 0xb8, 0x4b, 0x7d,
	})
	remoteRevocationBasePoint, _ = btcec.ParsePubKey([]byte{
		0x2,
		0x6c, 0x39, 0xa3, 0x6d, 0x93, 0x69, 0xac, 0x14,
		0x1f, 0xbb, 0x4, 0x86, 0x3, 0x82, 0x5, 0xe2,
		0xcb, 0xb0, 0x62, 0x41, 0xa, 0x93, 0x3, 0x6c,
		0x8d, 0xc0, 0x42, 0x4d, 0x9e, 0x51, 0x9b, 0x36,
	})
	remotePaymentBasePoint, _ = btcec.ParsePubKey([]byte{
		0x3,
		0xab, 0x74, 0x1e, 0x83, 0x48, 0xe3, 0xb5, 0x6,
		0x25, 0x1c, 0x80, 0xe7, 0xf2, 0x3e, 0x7d, 0xb7,
		0x7a, 0xc7, 0xd, 0x6, 0x3b, 0xbc, 0x74, 0x96,
		0x8e, 0x9b, 0x2d, 0xd1, 0x42, 0x71, 0xa5, 0x2a,
	})
	remoteDelayBasePoint, _ = btcec.ParsePubKey([]byte{
		0x2,
		0x4b, 0xdd, 0x52, 0x46, 0x1b, 0x50, 0x89, 0xb9,
		0x49, 0x4, 0xf2, 0xd2, 0x98, 0x7d, 0x51, 0xa1,
		0xa6, 0x3f, 0x9b, 0xd0, 0x40, 0x7c, 0x93, 0x74,
		0x3b, 0x8c, 0x4d, 0x63, 0x32, 0x90, 0xa, 0xca,
	})
	remoteHtlcBasePoint, _ = btcec.ParsePubKey([]byte{
		0x3,
		0x5b, 0x8f, 0x4a, 0x71, 0x4c, 0x2e, 0x71, 0x14,
		0x86, 0x1f, 0x30, 0x96, 0xc0, 0xd4, 0x11, 0x76,
		0xf8, 0xc3, 0xfc, 0x7, 0x2d, 0x15, 0x99, 0x55,
		0x8, 0x69, 0xf6, 0x1, 0xa2, 0xcd, 0x6b, 0xa7,
	})

	// withAmtData if set, will result in the amount data of the revoked
	// commitment transactions also being stored in the new revocation log.
	// The value of this variable is set randomly in the init function of
	// this package.
	withAmtData bool
)

// setupTestLogs takes care of creating the related buckets and inserts testing
// records.
func setupTestLogs(db kvdb.Backend, c *mig26.OpenChannel,
	oldLogs, newLogs []mig.ChannelCommitment) error {

	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		// If the open channel is nil, only create the root
		// bucket and skip creating the channel bucket.
		if c == nil {
			_, err := tx.CreateTopLevelBucket(openChannelBucket)
			return err
		}

		// Create test buckets.
		chanBucket, err := mig25.CreateChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}

		// Save channel info.
		if err := mig26.PutChanInfo(chanBucket, c, false); err != nil {
			return fmt.Errorf("PutChanInfo got %w", err)
		}

		// Save revocation state.
		if err := putChanRevocationState(chanBucket, c); err != nil {
			return fmt.Errorf("putChanRevocationState got %w", err)
		}

		// Create old logs.
		err = writeOldRevocationLogs(chanBucket, oldLogs)
		if err != nil {
			return fmt.Errorf("write old logs: %w", err)
		}

		// Create new logs.
		return writeNewRevocationLogs(chanBucket, newLogs, !withAmtData)
	}, func() {})
}

// createTestChannel creates an OpenChannel using the specified nodePub and
// outpoint. If any of the params is nil, a random value is populated.
func createTestChannel(nodePub *btcec.PublicKey) *mig26.OpenChannel {
	// Create a random private key that's used to provide randomness.
	priv, _ := btcec.NewPrivateKey()

	// If passed public key is nil, use the random public key.
	if nodePub == nil {
		nodePub = priv.PubKey()
	}

	// Create a random channel point.
	var op wire.OutPoint
	copy(op.Hash[:], priv.Serialize())

	testProducer := shachain.NewRevocationProducer(op.Hash)
	store, _ := createTestStore()

	localCfg := mig.ChannelConfig{
		ChannelConstraints: mig.ChannelConstraints{
			DustLimit:        btcutil.Amount(354),
			MaxAcceptedHtlcs: 483,
			CsvDelay:         4,
		},
		MultiSigKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: localMusigKey,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 1,
				Index:  0,
			},
			PubKey: localRevocationBasePoint,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 2,
				Index:  0,
			},
			PubKey: localHtlcBasePoint,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 3,
				Index:  0,
			},
			PubKey: localPaymentBasePoint,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 4,
				Index:  0,
			},
			PubKey: localDelayBasePoint,
		},
	}

	remoteCfg := mig.ChannelConfig{
		ChannelConstraints: mig.ChannelConstraints{
			DustLimit:        btcutil.Amount(354),
			MaxAcceptedHtlcs: 483,
			CsvDelay:         4,
		},
		MultiSigKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: remoteMultiSigKey,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: remoteRevocationBasePoint,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: remoteHtlcBasePoint,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: remotePaymentBasePoint,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 0,
				Index:  0,
			},
			PubKey: remoteDelayBasePoint,
		},
	}

	c := &mig26.OpenChannel{
		OpenChannel: mig25.OpenChannel{
			OpenChannel: mig.OpenChannel{
				ChainHash:       testChainHash,
				IdentityPub:     nodePub,
				FundingOutpoint: op,
				LocalChanCfg:    localCfg,
				RemoteChanCfg:   remoteCfg,
				// Assign dummy values.
				RemoteCurrentRevocation: nodePub,
				RevocationProducer:      testProducer,
				RevocationStore:         store,
			},
			ChanType: testChanType,
		},
	}

	return c
}

// writeOldRevocationLogs saves an old revocation log to db.
func writeOldRevocationLogs(chanBucket kvdb.RwBucket,
	oldLogs []mig.ChannelCommitment) error {

	// Don't bother continue if the logs are empty.
	if len(oldLogs) == 0 {
		return nil
	}

	logBucket, err := chanBucket.CreateBucketIfNotExists(
		revocationLogBucketDeprecated,
	)
	if err != nil {
		return err
	}

	for _, c := range oldLogs {
		if err := putOldRevocationLog(logBucket, &c); err != nil {
			return err
		}
	}
	return nil
}

// writeNewRevocationLogs saves a new revocation log to db.
func writeNewRevocationLogs(chanBucket kvdb.RwBucket,
	oldLogs []mig.ChannelCommitment, noAmtData bool) error {

	// Don't bother continue if the logs are empty.
	if len(oldLogs) == 0 {
		return nil
	}

	logBucket, err := chanBucket.CreateBucketIfNotExists(
		revocationLogBucket,
	)
	if err != nil {
		return err
	}

	for _, c := range oldLogs {
		// NOTE: we just blindly write the output indexes to db here
		// whereas normally, we would find the correct indexes from the
		// old commit tx. We do this intentionally so we can
		// distinguish a newly created log from an already saved one.
		err := putRevocationLog(
			logBucket, &c, testOurIndex, testTheirIndex, noAmtData,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// createTestStore creates a revocation store and always saves the above
// defined two preimages into the store.
func createTestStore() (shachain.Store, error) {
	var p chainhash.Hash
	copy(p[:], preimage1)

	testStore := shachain.NewRevocationStore()
	if err := testStore.AddNextEntry(&p); err != nil {
		return nil, err
	}

	copy(p[:], preimage2)
	if err := testStore.AddNextEntry(&p); err != nil {
		return nil, err
	}

	return testStore, nil
}

// createNotStarted will setup a situation where we haven't started the
// migration for the channel. We use the legacy to denote whether to simulate a
// node with v0.15.0.
func createNotStarted(cdb kvdb.Backend, c *mig26.OpenChannel,
	legacy bool) error {

	var newLogs []mig.ChannelCommitment

	// Create test logs.
	oldLogs := []mig.ChannelCommitment{oldLog1, oldLog2}

	// Add a new log if the node is running with v0.15.0.
	if !legacy {
		newLogs = []mig.ChannelCommitment{newLog3}
	}
	return setupTestLogs(cdb, c, oldLogs, newLogs)
}

// createNotFinished will setup a situation where we have un-migrated logs and
// return the next migration height. We use the legacy to denote whether to
// simulate a node with v0.15.0.
func createNotFinished(cdb kvdb.Backend, c *mig26.OpenChannel,
	legacy bool) error {

	// Create test logs.
	oldLogs := []mig.ChannelCommitment{oldLog1, oldLog2}
	newLogs := []mig.ChannelCommitment{oldLog1}

	// Add a new log if the node is running with v0.15.0.
	if !legacy {
		newLogs = append(newLogs, newLog3)
	}
	return setupTestLogs(cdb, c, oldLogs, newLogs)
}

// createFinished will setup a situation where all the old logs have been
// migrated and return a nil. We use the legacy to denote whether to simulate a
// node with v0.15.0.
func createFinished(cdb kvdb.Backend, c *mig26.OpenChannel,
	legacy bool) error {

	// Create test logs.
	oldLogs := []mig.ChannelCommitment{oldLog1, oldLog2}
	newLogs := []mig.ChannelCommitment{oldLog1, oldLog2}

	// Add a new log if the node is running with v0.15.0.
	if !legacy {
		newLogs = append(newLogs, newLog3)
	}
	return setupTestLogs(cdb, c, oldLogs, newLogs)
}

func init() {
	rand.Seed(time.Now().Unix())
	if rand.Intn(2) == 0 {
		withAmtData = true
	}

	if withAmtData {
		newLog1 = newLog1WithAmts
		newLog2 = newLog2WithAmts
	}
}
