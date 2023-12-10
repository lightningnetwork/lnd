package migration32

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (
	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571" +
		"319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a" +
		"88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)

	sig, _ = lnwire.NewSigFromSignature(testSig)

	wp1 = &WaitingProof{
		AnnounceSignatures: &lnwire.AnnounceSignatures{
			ChannelID:        lnwire.ChannelID{1},
			ShortChannelID:   lnwire.NewShortChanIDFromInt(1),
			NodeSignature:    sig,
			BitcoinSignature: sig,
			ExtraOpaqueData:  []byte{1, 2, 3, 4},
		},
		isRemote: false,
	}

	wp2 = &WaitingProof{
		AnnounceSignatures: &lnwire.AnnounceSignatures{
			ChannelID:        lnwire.ChannelID{2},
			ShortChannelID:   lnwire.NewShortChanIDFromInt(2),
			NodeSignature:    sig,
			BitcoinSignature: sig,
		},
		isRemote: true,
	}
)

// TestMigrationWaitingProofStore tests that the MigrateWaitingProofStore
// function works as expected.
func TestMigrateWaitingProofStore(t *testing.T) {
	var (
		key1           = wp1.Key()
		key2           = wp2.Key()
		wp1BytesBefore bytes.Buffer
		wp2BytesBefore bytes.Buffer
		wp1BytesAfter  bytes.Buffer
		wp2BytesAfter  bytes.Buffer
	)

	err := wp1.LegacyEncode(&wp1BytesBefore)
	require.NoError(t, err)

	err = wp2.LegacyEncode(&wp2BytesBefore)
	require.NoError(t, err)

	wpStoreBefore := map[string]interface{}{
		string(key1[:]): string(wp1BytesBefore.Bytes()),
		string(key2[:]): string(wp2BytesBefore.Bytes()),
	}

	err = wp1.UpdatedEncode(&wp1BytesAfter)
	require.NoError(t, err)

	err = wp2.UpdatedEncode(&wp2BytesAfter)
	require.NoError(t, err)

	wpStoreAfter := map[string]interface{}{
		string(key1[:]): string(wp1BytesAfter.Bytes()),
		string(key2[:]): string(wp2BytesAfter.Bytes()),
	}

	before := func(tx kvdb.RwTx) error {
		return migtest.RestoreDB(
			tx, waitingProofsBucketKey, wpStoreBefore,
		)
	}

	after := func(tx kvdb.RwTx) error {
		return migtest.VerifyDB(
			tx, waitingProofsBucketKey, wpStoreAfter,
		)
	}

	migtest.ApplyMigration(
		t, before, after, MigrateWaitingProofStore, false,
	)
}
