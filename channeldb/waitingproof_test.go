package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

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
