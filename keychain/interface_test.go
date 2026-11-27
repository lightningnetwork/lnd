package keychain

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Required in order to create the default database.
	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/require"
)

var (
	testHDSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x98, 0xa3, 0xef, 0xb9,
		0x69, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	// testDBTimeout is the wallet db timeout value used in this test.
	testDBTimeout = time.Second * 10
)

func createTestBtcWallet(t testing.TB, coinType uint32) (*wallet.Wallet, error) {
	// Instruct waddrmgr to use the cranked down scrypt parameters when
	// creating new wallet encryption keys.
	fastScrypt := waddrmgr.FastScryptOptions
	keyGen := func(passphrase *[]byte, config *waddrmgr.ScryptOptions) (
		*snacl.SecretKey, error) {

		return snacl.NewSecretKey(
			passphrase, fastScrypt.N, fastScrypt.R, fastScrypt.P,
		)
	}
	waddrmgr.SetSecretKeyGen(keyGen)

	// Create a new test wallet that uses fast scrypt as KDF.
	loader := wallet.NewLoader(
		&chaincfg.SimNetParams, t.TempDir(), true, testDBTimeout, 0,
	)

	pass := []byte("test")

	baseWallet, err := loader.CreateNewWallet(
		pass, pass, testHDSeed[:], time.Time{},
	)
	if err != nil {
		return nil, err
	}

	if err := baseWallet.Unlock(pass, nil); err != nil {
		return nil, err
	}

	// Construct the key scope required to derive keys for the chose
	// coinType.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: BIP0043Purpose,
		Coin:    coinType,
	}

	// We'll now ensure that the KeyScope: (1017, coinType) exists within
	// the internal waddrmgr. We'll need this in order to properly generate
	// the keys required for signing various contracts.
	_, err = baseWallet.Manager.FetchScopedKeyManager(chainKeyScope)
	if err != nil {
		err := walletdb.Update(baseWallet.Database(), func(tx walletdb.ReadWriteTx) error {
			addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

			_, err := baseWallet.Manager.NewScopedKeyManager(
				addrmgrNs, chainKeyScope, lightningAddrSchema,
			)
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	t.Cleanup(func() {
		baseWallet.Lock()
	})

	return baseWallet, nil
}

func assertEqualKeyLocator(t *testing.T, a, b KeyLocator) {
	t.Helper()
	if a != b {
		t.Fatalf("mismatched key locators: expected %v, "+
			"got %v", spew.Sdump(a), spew.Sdump(b))
	}
}

// secretKeyRingConstructor is a function signature that's used as a generic
// constructor for various implementations of the KeyRing interface. A string
// naming the returned interface, and the KeyRing interface itself are to be
// returned.
type keyRingConstructor func() (string, KeyRing, error)

// TestKeyRingDerivation tests that each known KeyRing implementation properly
// adheres to the expected behavior of the set of interfaces.
func TestKeyRingDerivation(t *testing.T) {
	t.Parallel()

	keyRingImplementations := []keyRingConstructor{
		func() (string, KeyRing, error) {
			wallet, err := createTestBtcWallet(t, CoinTypeBitcoin)
			require.NoError(t, err)

			keyRing := NewBtcWalletKeyRing(wallet, CoinTypeBitcoin)

			return "btcwallet", keyRing, nil
		},
		func() (string, KeyRing, error) {
			wallet, err := createTestBtcWallet(t, CoinTypeTestnet)
			require.NoError(t, err)

			keyRing := NewBtcWalletKeyRing(wallet, CoinTypeTestnet)

			return "testwallet", keyRing, nil
		},
	}

	const numKeysToDerive = 10

	// For each implementation constructor registered above, we'll execute
	// an identical set of tests in order to ensure that the interface
	// adheres to our nominal specification.
	for _, keyRingConstructor := range keyRingImplementations {
		keyRingName, keyRing, err := keyRingConstructor()
		if err != nil {
			t.Fatalf("unable to create key ring %v: %v", keyRingName,
				err)
		}

		success := t.Run(fmt.Sprintf("%v", keyRingName), func(t *testing.T) {
			// First, we'll ensure that we're able to derive keys
			// from each of the known key families.
			for _, keyFam := range VersionZeroKeyFamilies {
				// First, we'll ensure that we can derive the
				// *next* key in the keychain.
				keyDesc, err := keyRing.DeriveNextKey(keyFam)
				require.NoError(t, err)
				assertEqualKeyLocator(t,
					KeyLocator{
						Family: keyFam,
						Index:  0,
					}, keyDesc.KeyLocator,
				)

				// We'll now re-derive that key to ensure that
				// we're able to properly access the key via
				// the random access derivation methods.
				keyLoc := KeyLocator{
					Family: keyFam,
					Index:  0,
				}
				firstKeyDesc, err := keyRing.DeriveKey(keyLoc)
				require.NoError(t, err)
				if !keyDesc.PubKey.IsEqual(firstKeyDesc.PubKey) {
					t.Fatalf("mismatched keys: expected %x, "+
						"got %x",
						keyDesc.PubKey.SerializeCompressed(),
						firstKeyDesc.PubKey.SerializeCompressed())
				}
				assertEqualKeyLocator(t,
					KeyLocator{
						Family: keyFam,
						Index:  0,
					}, firstKeyDesc.KeyLocator,
				)

				// If we now try to manually derive the next 10
				// keys (including the original key), then we
				// should get an identical public key back and
				// their KeyLocator information
				// should be set properly.
				for i := 0; i < numKeysToDerive+1; i++ {
					keyLoc := KeyLocator{
						Family: keyFam,
						Index:  uint32(i),
					}
					keyDesc, err := keyRing.DeriveKey(keyLoc)
					require.NoError(t, err)

					// Ensure that the key locator matches
					// up as well.
					assertEqualKeyLocator(
						t, keyLoc, keyDesc.KeyLocator,
					)
				}

				// If this succeeds, then we'll also try to
				// derive a random index within the range.
				randKeyIndex := uint32(rand.Int31())
				keyLoc = KeyLocator{
					Family: keyFam,
					Index:  randKeyIndex,
				}
				keyDesc, err = keyRing.DeriveKey(keyLoc)
				require.NoError(t, err)
				assertEqualKeyLocator(
					t, keyLoc, keyDesc.KeyLocator,
				)
			}
		})
		if !success {
			break
		}
	}
}

// nextFamilyIndex returns the index that the next key derived for the given
// family will have, as recorded by the wallet.
func nextFamilyIndex(t *testing.T, w *wallet.Wallet, coinType uint32,
	keyFam KeyFamily) uint32 {

	t.Helper()

	props, err := w.AccountProperties(waddrmgr.KeyScope{
		Purpose: BIP0043Purpose,
		Coin:    coinType,
	}, uint32(keyFam))
	require.NoError(t, err)

	return props.ExternalKeyCount
}

// assertKnownToWallet asserts whether the wallet is able to map the given key
// back to the address it belongs to, which is a prerequisite for the wallet
// being able to sign with that key.
func assertKnownToWallet(t *testing.T, w *wallet.Wallet, desc KeyDescriptor,
	known bool) {

	t.Helper()

	addr, err := address.NewAddressWitnessPubKeyHash(
		address.Hash160(desc.PubKey.SerializeCompressed()),
		&chaincfg.SimNetParams,
	)
	require.NoError(t, err)

	managedAddr, err := w.AddressInfo(addr)
	if !known {
		require.Error(t, err)

		return
	}

	require.NoError(t, err)

	pubKeyAddr, ok := managedAddr.(waddrmgr.ManagedPubKeyAddress)
	require.True(t, ok)

	_, path, ok := pubKeyAddr.DerivationInfo()
	require.True(t, ok)
	require.Equal(t, uint32(desc.Family), path.InternalAccount)
	require.Equal(t, desc.Index, path.Index)
}

// TestDeriveAndStoreKey tests that DeriveAndStoreKey records the derived key in
// the wallet, advances the key family's derivation index past it, and bounds
// the number of keys a single call derives.
func TestDeriveAndStoreKey(t *testing.T) {
	t.Parallel()

	const (
		coinType = CoinTypeBitcoin
		keyFam   = KeyFamilyNodeKey
	)

	baseWallet, err := createTestBtcWallet(t, coinType)
	require.NoError(t, err)

	keyRing := NewBtcWalletKeyRing(baseWallet, coinType)

	// Storing a key derives and records every key in the family up to and
	// including the requested index, so the family's next index moves past
	// it.
	desc, err := keyRing.DeriveAndStoreKey(KeyLocator{
		Family: keyFam,
		Index:  5,
	})
	require.NoError(t, err)
	assertEqualKeyLocator(t, KeyLocator{
		Family: keyFam,
		Index:  5,
	}, desc.KeyLocator)
	require.EqualValues(
		t, 6, nextFamilyIndex(t, baseWallet, coinType, keyFam),
	)

	// Which means that the indexes we just consumed are not handed out
	// again. This is the property that lets a caller restore a key family's
	// index after the wallet was recovered from seed.
	nextDesc, err := keyRing.DeriveNextKey(keyFam)
	require.NoError(t, err)
	require.EqualValues(t, 6, nextDesc.Index)

	// The key itself must be the very same one that the plain DeriveKey
	// method returns.
	plainDesc, err := keyRing.DeriveKey(KeyLocator{
		Family: keyFam,
		Index:  5,
	})
	require.NoError(t, err)
	require.True(t, plainDesc.PubKey.IsEqual(desc.PubKey))

	// But unlike DeriveKey, the wallet is now able to map the key back to
	// its address, and therefore to its key locator, which it needs in
	// order to sign with the key.
	assertKnownToWallet(t, baseWallet, desc, true)

	unstoredDesc, err := keyRing.DeriveKey(KeyLocator{
		Family: keyFam,
		Index:  50,
	})
	require.NoError(t, err)
	assertKnownToWallet(t, baseWallet, unstoredDesc, false)

	// Storing a key at an index the family has already advanced past leaves
	// the wallet untouched, the index is never rewound.
	earlierDesc, err := keyRing.DeriveAndStoreKey(KeyLocator{
		Family: keyFam,
		Index:  2,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, earlierDesc.Index)
	require.EqualValues(
		t, 7, nextFamilyIndex(t, baseWallet, coinType, keyFam),
	)

	// An index that is too far ahead of the family's next index is refused
	// outright instead of deriving an unbounded number of keys, and the
	// family is left untouched.
	_, err = keyRing.DeriveAndStoreKey(KeyLocator{
		Family: keyFam,
		Index:  7 + MaxKeyIndexExtension,
	})
	require.ErrorIs(t, err, ErrKeyExtensionTooLarge)
	require.EqualValues(
		t, 7, nextFamilyIndex(t, baseWallet, coinType, keyFam),
	)
}

// secretKeyRingConstructor is a function signature that's used as a generic
// constructor for various implementations of the SecretKeyRing interface. A
// string naming the returned interface, and the SecretKeyRing interface itself
// are to be returned.
type secretKeyRingConstructor func() (string, SecretKeyRing, error)

// TestSecretKeyRingDerivation tests that each known SecretKeyRing
// implementation properly adheres to the expected behavior of the set of
// interface.
func TestSecretKeyRingDerivation(t *testing.T) {
	t.Parallel()

	secretKeyRingImplementations := []secretKeyRingConstructor{
		func() (string, SecretKeyRing, error) {
			wallet, err := createTestBtcWallet(t, CoinTypeBitcoin)
			require.NoError(t, err)

			keyRing := NewBtcWalletKeyRing(wallet, CoinTypeBitcoin)

			return "btcwallet", keyRing, nil
		},
		func() (string, SecretKeyRing, error) {
			wallet, err := createTestBtcWallet(t, CoinTypeTestnet)
			require.NoError(t, err)

			keyRing := NewBtcWalletKeyRing(wallet, CoinTypeTestnet)

			return "testwallet", keyRing, nil
		},
	}

	// For each implementation constructor registered above, we'll execute
	// an identical set of tests in order to ensure that the interface
	// adheres to our nominal specification.
	for _, secretKeyRingConstructor := range secretKeyRingImplementations {
		keyRingName, secretKeyRing, err := secretKeyRingConstructor()
		if err != nil {
			t.Fatalf("unable to create secret key ring %v: %v",
				keyRingName, err)
		}

		success := t.Run(fmt.Sprintf("%v", keyRingName), func(t *testing.T) {
			// For, each key family, we'll ensure that we're able
			// to obtain the private key of a randomly select child
			// index within the key family.
			for _, keyFam := range VersionZeroKeyFamilies {
				randKeyIndex := uint32(rand.Int31())
				keyLoc := KeyLocator{
					Family: keyFam,
					Index:  randKeyIndex,
				}

				// First, we'll query for the public key for
				// this target key locator.
				pubKeyDesc, err := secretKeyRing.DeriveKey(keyLoc)
				if err != nil {
					t.Fatalf("unable to derive pubkey "+
						"(fam=%v, index=%v): %v",
						keyLoc.Family,
						keyLoc.Index, err)
				}

				// With the public key derive, ensure that
				// we're able to obtain the corresponding
				// private key correctly.
				privKey, err := secretKeyRing.DerivePrivKey(KeyDescriptor{
					KeyLocator: keyLoc,
				})
				if err != nil {
					t.Fatalf("unable to derive priv "+
						"(fam=%v, index=%v): %v", keyLoc.Family,
						keyLoc.Index, err)
				}

				// Finally, ensure that the keys match up
				// properly.
				if !pubKeyDesc.PubKey.IsEqual(privKey.PubKey()) {
					t.Fatalf("pubkeys mismatched: expected %x, got %x",
						pubKeyDesc.PubKey.SerializeCompressed(),
						privKey.PubKey().SerializeCompressed())
				}

				// Next, we'll test that we're able to derive a
				// key given only the public key and key
				// family.
				//
				// Derive a new key from the key ring.
				keyDesc, err := secretKeyRing.DeriveNextKey(keyFam)
				if err != nil {
					t.Fatalf("unable to derive key: %v", err)
				}

				// We'll now construct a key descriptor that
				// requires us to scan the key range, and query
				// for the key, we should be able to find it as
				// it's valid.
				keyDesc = KeyDescriptor{
					PubKey: keyDesc.PubKey,
					KeyLocator: KeyLocator{
						Family: keyFam,
					},
				}
				privKey, err = secretKeyRing.DerivePrivKey(keyDesc)
				if err != nil {
					t.Fatalf("unable to derive priv key "+
						"via scanning: %v", err)
				}

				// Having to resort to scanning, we should be
				// able to find the target public key.
				if !keyDesc.PubKey.IsEqual(privKey.PubKey()) {
					t.Fatalf("pubkeys mismatched: expected %x, got %x",
						pubKeyDesc.PubKey.SerializeCompressed(),
						privKey.PubKey().SerializeCompressed())
				}

				// We'll try again, but this time with an
				// unknown public key.
				_, pub := btcec.PrivKeyFromBytes(
					testHDSeed[:],
				)
				keyDesc.PubKey = pub

				// If we attempt to query for this key, then we
				// should get ErrCannotDerivePrivKey.
				_, err = secretKeyRing.DerivePrivKey(
					keyDesc,
				)
				if err != ErrCannotDerivePrivKey {
					t.Fatalf("expected %T, instead got %v",
						ErrCannotDerivePrivKey, err)
				}

				// TODO(roasbeef): scalar mult once integrated
			}
		})
		if !success {
			break
		}
	}
}

func init() {
	// We'll clamp the max range scan to constrain the run time of the
	// private key scan test.
	MaxKeyRangeScan = 3
}
