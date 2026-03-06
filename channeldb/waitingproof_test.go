package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWaitingProofStore tests add/get/remove functions of the waiting proof
// storage.
func TestWaitingProofStore(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "failed to make test database")

	proof1 := NewWaitingProof(true, &lnwire.AnnounceSignatures1{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
		ExtraOpaqueData:  make([]byte, 0),
	})

	store, err := NewWaitingProofStore(db)
	if err != nil {
		t.Fatalf("unable to create the waiting proofs storage: %v",
			err)
	}

	if err := store.Add(proof1); err != nil {
		t.Fatalf("unable add proof to storage: %v", err)
	}

	proof2, err := store.Get(proof1.Key())
	require.NoError(t, err, "unable retrieve proof from storage")
	if !reflect.DeepEqual(proof1, proof2) {
		t.Fatalf("wrong proof retrieved: expected %v, got %v",
			spew.Sdump(proof1), spew.Sdump(proof2))
	}

	if _, err := store.Get(proof1.OppositeKey()); err != ErrWaitingProofNotFound {
		t.Fatalf("proof shouldn't be found: %v", err)
	}

	if err := store.Remove(proof1.Key()); err != nil {
		t.Fatalf("unable remove proof from storage: %v", err)
	}

	if err := store.ForAll(func(proof *WaitingProof) error {
		return errors.New("storage should be empty")
	}, func() {}); err != nil && err != ErrWaitingProofNotFound {
		t.Fatal(err)
	}
}

// TestWaitingProofEncodePrefix asserts that waiting proofs are encoded with the
// V1 waiting proof type prefix.
func TestWaitingProofEncodePrefix(t *testing.T) {
	t.Parallel()

	proof := NewWaitingProof(true, &lnwire.AnnounceSignatures1{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
		ExtraOpaqueData:  []byte{1, 2, 3},
	})

	var encoded bytes.Buffer
	require.NoError(t, proof.Encode(&encoded))

	var proofType WaitingProofType
	require.NoError(t, binary.Read(&encoded, byteOrder, &proofType))
	require.Equal(t, WaitingProofTypeV1, proofType)
}

// TestWaitingProofDecodeUnknownType asserts that decoding fails for unknown
// waiting proof type prefixes.
func TestWaitingProofDecodeUnknownType(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, binary.Write(&encoded, byteOrder, uint8(99)))
	require.NoError(t, binary.Write(&encoded, byteOrder, true))

	msg := &lnwire.AnnounceSignatures1{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
	}
	require.NoError(t, msg.Encode(&encoded, 0))

	var proof WaitingProof
	err := proof.Decode(&encoded)
	require.ErrorContains(t, err, "unknown waiting proof type")
}

// TestWaitingProofV2RoundTrip asserts that a V2 waiting proof can be encoded
// and decoded correctly, both with and without an aggregate nonce.
func TestWaitingProofV2RoundTrip(t *testing.T) {
	t.Parallel()

	partialSig := lnwire.NewPartialSig(*testRScalar)

	annSig2 := lnwire.NewAnnSigs2(
		lnwire.ChannelID{1, 2, 3},
		lnwire.NewShortChanIDFromInt(42),
		partialSig,
	)

	// Generate a deterministic public key for the aggregate nonce.
	aggNonce := pubKey

	testCases := []struct {
		name     string
		aggNonce *btcec.PublicKey
	}{
		{
			name:     "with agg nonce",
			aggNonce: aggNonce,
		},
		{
			name:     "without agg nonce",
			aggNonce: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proof := NewV2WaitingProof(
				true, annSig2, tc.aggNonce,
			)

			var buf bytes.Buffer
			require.NoError(t, proof.Encode(&buf))

			// Verify the type prefix is V2.
			var proofType WaitingProofType
			r := bytes.NewReader(buf.Bytes())
			require.NoError(t, binary.Read(
				r, byteOrder, &proofType,
			))
			require.Equal(t, WaitingProofTypeV2, proofType)

			// Decode and compare.
			var decoded WaitingProof
			require.NoError(t, decoded.Decode(
				bytes.NewReader(buf.Bytes()),
			))

			require.Equal(t, proof.isRemote, decoded.isRemote)
			require.Equal(t, proof.Key(), decoded.Key())

			inner := decoded.WaitingProofInner
			decodedV2, ok := inner.(*V2WaitingProof)
			require.True(t, ok)

			origInner := proof.WaitingProofInner
			origV2, ok := origInner.(*V2WaitingProof)
			require.True(t, ok)

			require.Equal(
				t,
				origV2.ShortChannelID.Val,
				decodedV2.ShortChannelID.Val,
			)
			require.Equal(
				t,
				origV2.ChannelID.Val,
				decodedV2.ChannelID.Val,
			)

			if tc.aggNonce != nil {
				require.NotNil(t, decodedV2.AggNonce)
				require.True(
					t,
					tc.aggNonce.IsEqual(
						decodedV2.AggNonce,
					),
				)
			} else {
				require.Nil(t, decodedV2.AggNonce)
			}
		})
	}
}

// TestWaitingProofV2Store tests add/get/remove of V2 waiting proofs through
// the store.
func TestWaitingProofV2Store(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err)

	store, err := NewWaitingProofStore(db)
	require.NoError(t, err)

	partialSig := lnwire.NewPartialSig(*testRScalar)
	annSig2 := lnwire.NewAnnSigs2(
		lnwire.ChannelID{5, 6, 7},
		lnwire.NewShortChanIDFromInt(100),
		partialSig,
	)

	proof := NewV2WaitingProof(true, annSig2, pubKey)

	require.NoError(t, store.Add(proof))

	got, err := store.Get(proof.Key())
	require.NoError(t, err)
	require.Equal(t, proof.Key(), got.Key())
	require.Equal(t, proof.isRemote, got.isRemote)

	gotV2, ok := got.WaitingProofInner.(*V2WaitingProof)
	require.True(t, ok)
	require.True(t, pubKey.IsEqual(gotV2.AggNonce))

	require.NoError(t, store.Remove(proof.Key()))

	_, err = store.Get(proof.Key())
	require.ErrorIs(t, err, ErrWaitingProofNotFound)
}

// TestWaitingProofCrossVersionKeyIsolation asserts that V1 and V2 waiting
// proofs for the same channel side are keyed independently.
func TestWaitingProofCrossVersionKeyIsolation(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err)

	store, err := NewWaitingProofStore(db)
	require.NoError(t, err)

	scid := lnwire.NewShortChanIDFromInt(777)

	v1Proof := NewWaitingProof(true, &lnwire.AnnounceSignatures1{
		ShortChannelID:   scid,
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
		ExtraOpaqueData:  []byte{1},
	})

	partialSig := lnwire.NewPartialSig(*testRScalar)
	v2AnnSig := lnwire.NewAnnSigs2(
		lnwire.ChannelID{9, 9, 9},
		scid,
		partialSig,
	)
	v2Proof := NewV2WaitingProof(true, v2AnnSig, pubKey)

	require.NotEqual(t, v1Proof.Key(), v2Proof.Key())

	require.NoError(t, store.Add(v1Proof))
	require.NoError(t, store.Add(v2Proof))

	gotV1, err := store.Get(v1Proof.Key())
	require.NoError(t, err)
	_, ok := gotV1.WaitingProofInner.(*V1WaitingProof)
	require.True(t, ok)

	gotV2, err := store.Get(v2Proof.Key())
	require.NoError(t, err)
	gotV2Inner, ok := gotV2.WaitingProofInner.(*V2WaitingProof)
	require.True(t, ok)
	require.True(t, pubKey.IsEqual(gotV2Inner.AggNonce))
}
