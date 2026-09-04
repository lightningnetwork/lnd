package keychain

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// newTestBtcWalletKeyRing spins up a btcwallet-backed keyring and returns it as
// the concrete *BtcWalletKeyRing type so the unexported ECDH helpers can be
// exercised directly.
func newTestBtcWalletKeyRing(t *testing.T) *BtcWalletKeyRing {
	t.Helper()

	wallet, err := createTestBtcWallet(t, CoinTypeBitcoin)
	require.NoError(t, err)

	keyRing, ok := NewBtcWalletKeyRing(
		wallet, CoinTypeBitcoin,
	).(*BtcWalletKeyRing)
	require.True(t, ok, "expected *BtcWalletKeyRing")

	return keyRing
}

// TestDerivePrivKeyForECDHCaching ensures that derivePrivKeyForECDH returns the
// private key matching the descriptor's public key, populates the cache on the
// first call, and serves subsequent calls from the cache while handing back an
// independent copy.
func TestDerivePrivKeyForECDHCaching(t *testing.T) {
	t.Parallel()

	keyRing := newTestBtcWalletKeyRing(t)

	keyDesc, err := keyRing.DeriveKey(KeyLocator{
		Family: KeyFamilyNodeKey,
		Index:  0,
	})
	require.NoError(t, err)
	require.NotNil(t, keyDesc.PubKey)

	// The cache starts empty.
	require.Equal(t, 0, keyRing.ecdhPrivKeyCache.Len())

	// The first call derives the key and must return the private key that
	// corresponds to the descriptor's public key.
	priv, err := keyRing.derivePrivKeyForECDH(keyDesc)
	require.NoError(t, err)
	require.True(t, priv.PubKey().IsEqual(keyDesc.PubKey))

	// The derived key is now cached.
	require.Equal(t, 1, keyRing.ecdhPrivKeyCache.Len())

	// A second call returns the same key material, but as an independent
	// copy so a caller mutating it can't corrupt the cached entry.
	priv2, err := keyRing.derivePrivKeyForECDH(keyDesc)
	require.NoError(t, err)
	require.Equal(t, priv.Serialize(), priv2.Serialize())
	require.NotSame(t, priv, priv2)

	// The cache copy must also be distinct from what we hand back.
	var cacheKey [btcec.PubKeyBytesLenCompressed]byte
	copy(cacheKey[:], keyDesc.PubKey.SerializeCompressed())
	cached, err := keyRing.ecdhPrivKeyCache.Get(cacheKey)
	require.NoError(t, err)
	require.NotSame(t, &cached.key, priv2)
}

// TestDerivePrivKeyForECDHNoPubKey ensures that descriptors without a public
// key bypass the cache and forward to DerivePrivKey directly.
func TestDerivePrivKeyForECDHNoPubKey(t *testing.T) {
	t.Parallel()

	keyRing := newTestBtcWalletKeyRing(t)

	keyLoc := KeyLocator{
		Family: KeyFamilyNodeKey,
		Index:  0,
	}

	priv, err := keyRing.derivePrivKeyForECDH(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	require.NoError(t, err)

	// The no-PubKey path must not populate the cache.
	require.Equal(t, 0, keyRing.ecdhPrivKeyCache.Len())

	// It must return the same key DerivePrivKey would.
	want, err := keyRing.DerivePrivKey(KeyDescriptor{KeyLocator: keyLoc})
	require.NoError(t, err)
	require.Equal(t, want.Serialize(), priv.Serialize())
}

// TestDerivePrivKeyForECDHConsistentWithDerivePrivKey ensures the cached helper
// always agrees with a direct DerivePrivKey, both on the deriving call and on
// subsequent cache hits.
func TestDerivePrivKeyForECDHConsistentWithDerivePrivKey(t *testing.T) {
	t.Parallel()

	keyRing := newTestBtcWalletKeyRing(t)

	for _, fam := range []KeyFamily{
		KeyFamilyNodeKey, KeyFamilyMultiSig, KeyFamilyBaseEncryption,
	} {
		keyDesc, err := keyRing.DeriveKey(KeyLocator{
			Family: fam,
			Index:  0,
		})
		require.NoError(t, err)

		want, err := keyRing.DerivePrivKey(keyDesc)
		require.NoError(t, err)

		// Deriving call.
		got, err := keyRing.derivePrivKeyForECDH(keyDesc)
		require.NoError(t, err)
		require.Equal(t, want.Serialize(), got.Serialize())

		// Cache hit.
		got, err = keyRing.derivePrivKeyForECDH(keyDesc)
		require.NoError(t, err)
		require.Equal(t, want.Serialize(), got.Serialize())
	}
}

// TestDerivePrivKeyForECDHZeroesOnEvict ensures the delete callback wipes the
// cache's copy of the private key when an entry leaves the cache.
func TestDerivePrivKeyForECDHZeroesOnEvict(t *testing.T) {
	t.Parallel()

	keyRing := newTestBtcWalletKeyRing(t)

	keyDesc, err := keyRing.DeriveKey(KeyLocator{
		Family: KeyFamilyNodeKey,
		Index:  0,
	})
	require.NoError(t, err)

	_, err = keyRing.derivePrivKeyForECDH(keyDesc)
	require.NoError(t, err)

	var cacheKey [btcec.PubKeyBytesLenCompressed]byte
	copy(cacheKey[:], keyDesc.PubKey.SerializeCompressed())

	// Grab the actual cached entry, confirm it holds key material, then
	// evict it and confirm the callback zeroed that material.
	cached, err := keyRing.ecdhPrivKeyCache.Get(cacheKey)
	require.NoError(t, err)
	require.NotEqual(t, make([]byte, 32), cached.key.Serialize())

	keyRing.ecdhPrivKeyCache.Delete(cacheKey)
	require.Equal(t, make([]byte, 32), cached.key.Serialize())
}
