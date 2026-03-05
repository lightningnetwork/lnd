package migration35

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
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
	testECDSA   = ecdsa.NewSignature(testRScalar, testSScalar)
	testSig, _  = lnwire.NewSigFromSignature(testECDSA)
)

// encodeLegacyProof encodes a waiting proof in the pre-migration format.
func encodeLegacyProof(p *waitingProof) []byte {
	var b bytes.Buffer

	err := binary.Write(&b, byteOrder, p.isRemote)
	if err != nil {
		panic(err)
	}

	err = p.announceSignatures.Encode(&b, 0)
	if err != nil {
		panic(err)
	}

	return b.Bytes()
}

// newAnnSig creates a deterministic announce signatures test message.
func newAnnSig(scid uint64, chanIDByte byte) *lnwire.AnnounceSignatures {
	return &lnwire.AnnounceSignatures{
		ChannelID:        lnwire.ChannelID{chanIDByte},
		ShortChannelID:   lnwire.NewShortChanIDFromInt(scid),
		NodeSignature:    testSig,
		BitcoinSignature: testSig,
		ExtraOpaqueData:  []byte{chanIDByte, 1, 2, 3},
	}
}

// makeHappyPathSetup creates pre- and post-migration callbacks for the
// successful migration test case.
func makeHappyPathSetup() (func(tx kvdb.RwTx) error,
	func(tx kvdb.RwTx) error) {

	proof1 := &waitingProof{
		announceSignatures: newAnnSig(10, 1),
		isRemote:           false,
	}
	proof2 := &waitingProof{
		announceSignatures: newAnnSig(11, 2),
		isRemote:           true,
	}

	key1 := proof1.Key()
	key2 := proof2.Key()

	before := func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(waitingProofsBucketKey)
		if err != nil {
			return err
		}

		err = bucket.Put(key1[:], encodeLegacyProof(proof1))
		if err != nil {
			return err
		}

		return bucket.Put(key2[:], encodeLegacyProof(proof2))
	}

	expected := map[waitingProofKey]*waitingProof{
		key1: proof1,
		key2: proof2,
	}

	after := func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(waitingProofsBucketKey)
		if bucket == nil {
			return fmt.Errorf("waiting proofs bucket not found")
		}

		for key, proof := range expected {
			migrated := bucket.Get(key[:])
			if migrated == nil {
				return fmt.Errorf("migrated key %x "+
					"not found", key)
			}

			expectedBytes, err := encodeUpdatedWaitingProof(proof)
			if err != nil {
				return err
			}

			if !bytes.Equal(migrated, expectedBytes) {
				return fmt.Errorf("unexpected "+
					"migrated bytes for "+
					"key %x", key)
			}
		}

		return nil
	}

	return before, after
}

// makeKeyMismatchSetup creates pre- and post-migration callbacks for the key
// mismatch failure case.
func makeKeyMismatchSetup() (func(tx kvdb.RwTx) error,
	func(tx kvdb.RwTx) error) {

	proof := &waitingProof{
		announceSignatures: newAnnSig(15, 4),
		isRemote:           false,
	}
	wrongKey := waitingProofKey{}
	binary.BigEndian.PutUint64(wrongKey[:8], 99)

	before := func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(waitingProofsBucketKey)
		if err != nil {
			return err
		}

		return bucket.Put(wrongKey[:], encodeLegacyProof(proof))
	}

	after := func(tx kvdb.RwTx) error {
		return nil
	}

	return before, after
}

// TestMigrateWaitingProofStore verifies the waiting proof migration behavior.
func TestMigrateWaitingProofStore(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		setup func() (
			func(tx kvdb.RwTx) error, func(tx kvdb.RwTx) error,
		)
		shouldFail bool
	}{
		{
			name:       "happy path",
			setup:      makeHappyPathSetup,
			shouldFail: false,
		},
		{
			name:       "key mismatch fails",
			setup:      makeKeyMismatchSetup,
			shouldFail: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			before, after := tc.setup()
			migtest.ApplyMigration(
				t, before, after, MigrateWaitingProofStore,
				tc.shouldFail,
			)
		})
	}
}
