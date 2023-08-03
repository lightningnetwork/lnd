package keychain

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
				privKey, err = secretKeyRing.DerivePrivKey(
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
