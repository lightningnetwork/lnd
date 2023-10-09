package channeldb

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWaitingProofStore tests add/get/remove functions of the waiting proof
// storage.
func TestWaitingProofStore(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "failed to make test database")

	proof1 := NewLegacyWaitingProof(true, &lnwire.AnnounceSignatures{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: 1000,
		},
		ExtraOpaqueData: make([]byte, 0),
	})

	// No agg nonce.
	proof2 := NewTaprootWaitingProof(true, &lnwire.AnnouncementSignatures2{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: 2000,
		},
		PartialSignature: *randPartialSig(t),
		ExtraOpaqueData:  make([]byte, 0),
	}, nil)

	// With agg nonce.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	proof3 := NewTaprootWaitingProof(true, &lnwire.AnnouncementSignatures2{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: 2000,
		},
		PartialSignature: *randPartialSig(t),
		ExtraOpaqueData:  make([]byte, 0),
	}, priv.PubKey())

	proofs := []*WaitingProof{
		proof1,
		proof2,
		proof3,
	}

	store, err := NewWaitingProofStore(db)
	require.NoError(t, err)

	for _, proof := range proofs {
		require.NoError(t, store.Add(proof))

		p2, err := store.Get(proof.Key())
		require.NoError(t, err, "unable retrieve proof from storage")
		require.Equal(t, proof, p2)

		_, err = store.Get(proof.OppositeKey())
		require.ErrorIs(t, err, ErrWaitingProofNotFound)

		err = store.Remove(proof.Key())
		require.NoError(t, err)
	}

	if err := store.ForAll(func(proof *WaitingProof) error {
		return errors.New("storage should be empty")
	}, func() {}); err != nil && err != ErrWaitingProofNotFound {
		t.Fatal(err)
	}
}

func randPartialSig(t *testing.T) *lnwire.PartialSig {
	var sigBytes [32]byte
	_, err := rand.Read(sigBytes[:])
	require.NoError(t, err)

	var s btcec.ModNScalar
	s.SetByteSlice(sigBytes[:])

	return &lnwire.PartialSig{
		Sig: s,
	}
}
