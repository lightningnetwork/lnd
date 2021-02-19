package migration21

import (
	"bytes"
	"fmt"
	"math/big"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migration21/common"
	"github.com/lightningnetwork/lnd/channeldb/migration21/current"
	"github.com/lightningnetwork/lnd/channeldb/migration21/legacy"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
)

var (
	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	_, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), key[:])

	wireSig, _ = lnwire.NewSigFromSignature(testSig)

	testSig = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}

	testCommitDiff = &common.CommitDiff{
		Commitment: common.ChannelCommitment{
			CommitTx:  testTx,
			CommitSig: make([]byte, 0),
		},
		CommitSig: &lnwire.CommitSig{
			ChanID:    lnwire.ChannelID(key),
			CommitSig: wireSig,
			HtlcSigs: []lnwire.Sig{
				wireSig,
				wireSig,
			},
		},
		LogUpdates: []common.LogUpdate{
			{
				LogIndex: 1,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     1,
					Amount: lnwire.NewMSatFromSatoshis(100),
					Expiry: 25,
				},
			},
			{
				LogIndex: 2,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     2,
					Amount: lnwire.NewMSatFromSatoshis(200),
					Expiry: 50,
				},
			},
		},
		OpenedCircuitKeys: []common.CircuitKey{},
		ClosedCircuitKeys: []common.CircuitKey{},
	}

	testNetworkResult = &common.NetworkResult{
		Msg:          testCommitDiff.CommitSig,
		Unencrypted:  true,
		IsResolution: true,
	}

	testChanCloseSummary = &common.ChannelCloseSummary{
		RemotePub:               pubKey,
		Capacity:                9,
		RemoteCurrentRevocation: pubKey,
		RemoteNextRevocation:    pubKey,
		LastChanSyncMsg: &lnwire.ChannelReestablish{
			LocalUnrevokedCommitPoint: pubKey,
		},
	}

	netResultKey = []byte{3}

	chanID = lnwire.NewChanIDFromOutPoint(&wire.OutPoint{})

	adds = []common.LogUpdate{
		{
			LogIndex: 0,
			UpdateMsg: &lnwire.UpdateAddHTLC{
				ChanID:      chanID,
				ID:          1,
				Amount:      100,
				Expiry:      1000,
				PaymentHash: [32]byte{0},
			},
		},
		{
			LogIndex: 1,
			UpdateMsg: &lnwire.UpdateAddHTLC{
				ChanID:      chanID,
				ID:          1,
				Amount:      101,
				Expiry:      1001,
				PaymentHash: [32]byte{1},
			},
		},
	}

	settleFails = []common.LogUpdate{
		{
			LogIndex: 2,
			UpdateMsg: &lnwire.UpdateFulfillHTLC{
				ChanID:          chanID,
				ID:              0,
				PaymentPreimage: [32]byte{0},
			},
		},
		{
			LogIndex: 3,
			UpdateMsg: &lnwire.UpdateFailHTLC{
				ChanID: chanID,
				ID:     1,
				Reason: []byte{},
			},
		},
	}
)

// TestMigrateDatabaseWireMessages tests that we're able to properly migrate
// all the wire messages in the database which are written without a length
// prefix in front of them. At the time this test was written we need to
// migrate three areas: open channel commit diffs, open channel unacked updates,
// and network results in the switch.
func TestMigrateDatabaseWireMessages(t *testing.T) {

	var pub [33]byte
	copy(pub[:], key[:])

	migtest.ApplyMigration(
		t,
		func(tx kvdb.RwTx) error {
			t.Helper()

			// First, we'll insert a new fake channel (well just
			// the commitment diff) at the expected location
			// on-disk.
			openChanBucket, err := tx.CreateTopLevelBucket(
				openChannelBucket,
			)
			if err != nil {
				return err
			}
			nodeBucket, err := openChanBucket.CreateBucket(pub[:])
			if err != nil {
				return err
			}
			chainBucket, err := nodeBucket.CreateBucket(key[:])
			if err != nil {
				return err
			}
			chanBucket, err := chainBucket.CreateBucket(key[:])
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = legacy.SerializeCommitDiff(&b, testCommitDiff)
			if err != nil {
				return err
			}

			err = chanBucket.Put(commitDiffKey, b.Bytes())
			if err != nil {
				return err
			}

			var logUpdateBuf bytes.Buffer
			err = legacy.SerializeLogUpdates(
				&logUpdateBuf, testCommitDiff.LogUpdates,
			)
			if err != nil {
				return err
			}

			// We'll re-use the same log updates to insert as a set
			// of un-acked and unsigned pending log updateas as well.
			err = chanBucket.Put(
				unsignedAckedUpdatesKey, logUpdateBuf.Bytes(),
			)
			if err != nil {
				return err
			}

			err = chanBucket.Put(
				remoteUnsignedLocalUpdatesKey, logUpdateBuf.Bytes(),
			)
			if err != nil {
				return err
			}

			// Next, we'll insert a sample closed channel summary
			// for the 2nd part of our migration.
			closedChanBucket, err := tx.CreateTopLevelBucket(
				closedChannelBucket,
			)
			if err != nil {
				return err
			}

			var summaryBuf bytes.Buffer
			err = legacy.SerializeChannelCloseSummary(
				&summaryBuf, testChanCloseSummary,
			)
			if err != nil {
				return err
			}

			err = closedChanBucket.Put(key[:], summaryBuf.Bytes())
			if err != nil {
				return err
			}

			// Create a few forwarding packages to migrate.
			for i := uint64(100); i < 200; i++ {
				shortChanID := lnwire.NewShortChanIDFromInt(i)
				packager := legacy.NewChannelPackager(shortChanID)
				fwdPkg := common.NewFwdPkg(shortChanID, 0, adds, settleFails)

				if err := packager.AddFwdPkg(tx, fwdPkg); err != nil {
					return err
				}
			}

			// Finally, we need to insert a sample network result
			// as well for the final component of our migration.
			var netResBuf bytes.Buffer
			err = legacy.SerializeNetworkResult(
				&netResBuf, testNetworkResult,
			)
			if err != nil {
				return err
			}

			networkResults, err := tx.CreateTopLevelBucket(
				networkResultStoreBucketKey,
			)
			if err != nil {
				return err
			}

			return networkResults.Put(
				netResultKey, netResBuf.Bytes(),
			)
		},
		func(tx kvdb.RwTx) error {
			t.Helper()

			// We'll now read the commit diff from disk using the
			// _new_ decoding method. This should match the commit
			// diff we inserted in the pre-migration step.
			openChanBucket := tx.ReadWriteBucket(openChannelBucket)
			nodeBucket := openChanBucket.NestedReadWriteBucket(
				pub[:],
			)
			chainBucket := nodeBucket.NestedReadWriteBucket(key[:])
			chanBucket := chainBucket.NestedReadWriteBucket(key[:])

			commitDiffBytes := chanBucket.Get(commitDiffKey)
			if commitDiffBytes == nil {
				return fmt.Errorf("no commit diff found")
			}

			newCommitDiff, err := current.DeserializeCommitDiff(
				bytes.NewReader(commitDiffBytes),
			)
			if err != nil {
				return fmt.Errorf("unable to decode commit "+
					"diff: %v", err)
			}

			if !reflect.DeepEqual(newCommitDiff, testCommitDiff) {
				return fmt.Errorf("diff mismatch: expected "+
					"%v, got %v", spew.Sdump(testCommitDiff),
					spew.Sdump(newCommitDiff))
			}

			// Next, we'll ensure that the un-acked updates match
			// up as well.
			updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
			if updateBytes == nil {
				return fmt.Errorf("no update bytes found")
			}

			newUpdates, err := current.DeserializeLogUpdates(
				bytes.NewReader(updateBytes),
			)
			if err != nil {
				return err
			}

			if !reflect.DeepEqual(
				newUpdates, testCommitDiff.LogUpdates,
			) {
				return fmt.Errorf("updates mismatch: expected "+
					"%v, got %v",
					spew.Sdump(testCommitDiff.LogUpdates),
					spew.Sdump(newUpdates))
			}

			updateBytes = chanBucket.Get(remoteUnsignedLocalUpdatesKey)
			if updateBytes == nil {
				return fmt.Errorf("no update bytes found")
			}

			newUpdates, err = current.DeserializeLogUpdates(
				bytes.NewReader(updateBytes),
			)
			if err != nil {
				return err
			}

			if !reflect.DeepEqual(
				newUpdates, testCommitDiff.LogUpdates,
			) {
				return fmt.Errorf("updates mismatch: expected "+
					"%v, got %v",
					spew.Sdump(testCommitDiff.LogUpdates),
					spew.Sdump(newUpdates))
			}

			// Next, we'll ensure that the inserted close channel
			// summary bytes also mach up with what we inserted in
			// the prior step.
			closedChanBucket := tx.ReadWriteBucket(
				closedChannelBucket,
			)
			if closedChannelBucket == nil {
				return fmt.Errorf("no closed channels found")
			}

			chanSummaryBytes := closedChanBucket.Get(key[:])
			newChanCloseSummary, err := current.DeserializeCloseChannelSummary(
				bytes.NewReader(chanSummaryBytes),
			)
			if err != nil {
				return err
			}

			testChanCloseSummary.RemotePub.Curve = nil
			testChanCloseSummary.RemoteCurrentRevocation.Curve = nil
			testChanCloseSummary.RemoteNextRevocation.Curve = nil
			testChanCloseSummary.LastChanSyncMsg.LocalUnrevokedCommitPoint.Curve = nil

			newChanCloseSummary.RemotePub.Curve = nil
			newChanCloseSummary.RemoteCurrentRevocation.Curve = nil
			newChanCloseSummary.RemoteNextRevocation.Curve = nil
			newChanCloseSummary.LastChanSyncMsg.LocalUnrevokedCommitPoint.Curve = nil

			if !reflect.DeepEqual(
				newChanCloseSummary, testChanCloseSummary,
			) {
				return fmt.Errorf("summary mismatch: expected "+
					"%v, got %v",
					spew.Sdump(testChanCloseSummary),
					spew.Sdump(newChanCloseSummary))
			}

			// Fetch all forwarding packages.
			for i := uint64(100); i < 200; i++ {
				shortChanID := lnwire.NewShortChanIDFromInt(i)
				packager := current.NewChannelPackager(shortChanID)

				fwdPkgs, err := packager.LoadFwdPkgs(tx)
				if err != nil {
					return err
				}

				if len(fwdPkgs) != 1 {
					return fmt.Errorf("expected 1 pkg")
				}

				og := common.NewFwdPkg(shortChanID, 0, adds, settleFails)

				// Check that we deserialized the packages correctly.
				if !reflect.DeepEqual(fwdPkgs[0], og) {
					return fmt.Errorf("res mismatch: expected "+
						"%v, got %v",
						spew.Sdump(fwdPkgs[0]),
						spew.Sdump(og))
				}
			}

			// Finally, we'll check the network results to ensure
			// that was migrated properly as well.
			networkResults := tx.ReadBucket(
				networkResultStoreBucketKey,
			)
			if networkResults == nil {
				return fmt.Errorf("no net results found")
			}

			netResBytes := networkResults.Get(netResultKey)
			if netResBytes == nil {
				return fmt.Errorf("no network res found")
			}

			newNetRes, err := current.DeserializeNetworkResult(
				bytes.NewReader(netResBytes),
			)
			if err != nil {
				return err
			}

			if !reflect.DeepEqual(newNetRes, testNetworkResult) {
				return fmt.Errorf("res mismatch: expected "+
					"%v, got %v",
					spew.Sdump(testNetworkResult),
					spew.Sdump(newNetRes))
			}

			return nil
		},
		MigrateDatabaseWireMessages,
		false,
	)
}
