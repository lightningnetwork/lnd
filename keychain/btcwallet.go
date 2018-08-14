package keychain

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	// CoinTypeBitcoin specifies the BIP44 coin type for Bitcoin key
	// derivation.
	CoinTypeBitcoin uint32 = 0

	// CoinTypeTestnet specifies the BIP44 coin type for all testnet key
	// derivation.
	CoinTypeTestnet = 1

	// CoinTypeLitecoin specifies the BIP44 coin type for Litecoin key
	// derivation.
	CoinTypeLitecoin = 2
)

var (
	// lightningAddrSchema is the scope addr schema for all keys that we
	// derive. We'll treat them all as p2wkh addresses, as atm we must
	// specify a particular type.
	lightningAddrSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}

	// waddrmgrNamespaceKey is the namespace key that the waddrmgr state is
	// stored within the top-level waleltdb buckets of btcwallet.
	waddrmgrNamespaceKey = []byte("waddrmgr")
)

// BtcWalletKeyRing is an implementation of both the KeyRing and SecretKeyRing
// interfaces backed by btcwallet's internal root waddrmgr. Internally, we'll
// be using a ScopedKeyManager to do all of our derivations, using the key
// scope and scope addr scehma defined above. Re-using the existing key scope
// construction means that all key derivation will be protected under the root
// seed of the wallet, making each derived key fully deterministic.
type BtcWalletKeyRing struct {
	// wallet is a pointer to the active instance of the btcwallet core.
	// This is required as we'll need to manually open database
	// transactions in order to derive addresses and lookup relevant keys
	wallet *wallet.Wallet

	// chainKeyScope defines the purpose and coin type to be used when generating
	// keys for this keyring.
	chainKeyScope waddrmgr.KeyScope

	// lightningScope is a pointer to the scope that we'll be using as a
	// sub key manager to derive all the keys that we require.
	lightningScope *waddrmgr.ScopedKeyManager
}

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The passed waddrmgr.Manager MUST be unlocked in order for the keychain
// to function.
func NewBtcWalletKeyRing(w *wallet.Wallet, coinType uint32) SecretKeyRing {
	// Construct the key scope that will be used within the waddrmgr to
	// create an HD chain for deriving all of our required keys. A different
	// scope is used for each specific coin type.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: BIP0043Purpose,
		Coin:    coinType,
	}

	return &BtcWalletKeyRing{
		wallet:        w,
		chainKeyScope: chainKeyScope,
	}
}

// keyScope attempts to return the key scope that we'll use to derive all of
// our keys. If the scope has already been fetched from the database, then a
// cached version will be returned. Otherwise, we'll fetch it from the database
// and cache it for subsequent accesses.
func (b *BtcWalletKeyRing) keyScope() (*waddrmgr.ScopedKeyManager, error) {
	// If the scope has already been populated, then we'll return it
	// directly.
	if b.lightningScope != nil {
		return b.lightningScope, nil
	}

	// Otherwise, we'll first do a check to ensure that the root manager
	// isn't locked, as otherwise we won't be able to *use* the scope.
	if b.wallet.Manager.IsLocked() {
		return nil, fmt.Errorf("cannot create BtcWalletKeyRing with " +
			"locked waddrmgr.Manager")
	}

	// If the manager is indeed unlocked, then we'll fetch the scope, cache
	// it, and return to the caller.
	lnScope, err := b.wallet.Manager.FetchScopedKeyManager(b.chainKeyScope)
	if err != nil {
		return nil, err
	}

	b.lightningScope = lnScope

	return lnScope, nil
}

// createAccountIfNotExists will create the corresponding account for a key
// family if it doesn't already exist in the database.
func (b *BtcWalletKeyRing) createAccountIfNotExists(
	addrmgrNs walletdb.ReadWriteBucket, keyFam KeyFamily,
	scope *waddrmgr.ScopedKeyManager) error {

	// If this is the multi-sig key family, then we can return early as
	// this is the default account that's created.
	if keyFam == KeyFamilyMultiSig {
		return nil
	}

	// Otherwise, we'll check if the account already exists, if so, we can
	// once again bail early.
	_, err := scope.AccountName(addrmgrNs, uint32(keyFam))
	if err == nil {
		return nil
	}

	// If we reach this point, then the account hasn't yet been created, so
	// we'll need to create it before we can proceed.
	return scope.NewRawAccount(addrmgrNs, uint32(keyFam))
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *BtcWalletKeyRing) DeriveNextKey(keyFam KeyFamily) (KeyDescriptor, error) {
	var (
		pubKey *btcec.PublicKey
		keyLoc KeyLocator
	)

	db := b.wallet.Database()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		scope, err := b.keyScope()
		if err != nil {
			return err
		}

		// If the account doesn't exist, then we may need to create it
		// for the first time in order to derive the keys that we
		// require.
		err = b.createAccountIfNotExists(addrmgrNs, keyFam, scope)
		if err != nil {
			return err
		}

		addrs, err := scope.NextExternalAddresses(
			addrmgrNs, uint32(keyFam), 1,
		)
		if err != nil {
			return err
		}

		// Extract the first address, ensuring that it is of the proper
		// interface type, otherwise we can't manipulate it below.
		addr, ok := addrs[0].(waddrmgr.ManagedPubKeyAddress)
		if !ok {
			return fmt.Errorf("address is not a managed pubkey " +
				"addr")
		}

		pubKey = addr.PubKey()

		_, pathInfo, _ := addr.DerivationInfo()
		keyLoc = KeyLocator{
			Family: keyFam,
			Index:  pathInfo.Index,
		}

		return nil
	})
	if err != nil {
		return KeyDescriptor{}, err
	}

	return KeyDescriptor{
		PubKey:     pubKey,
		KeyLocator: keyLoc,
	}, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *BtcWalletKeyRing) DeriveKey(keyLoc KeyLocator) (KeyDescriptor, error) {
	var keyDesc KeyDescriptor

	db := b.wallet.Database()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		scope, err := b.keyScope()
		if err != nil {
			return err
		}

		// If the account doesn't exist, then we may need to create it
		// for the first time in order to derive the keys that we
		// require.
		err = b.createAccountIfNotExists(addrmgrNs, keyLoc.Family, scope)
		if err != nil {
			return err
		}

		path := waddrmgr.DerivationPath{
			Account: uint32(keyLoc.Family),
			Branch:  0,
			Index:   uint32(keyLoc.Index),
		}
		addr, err := scope.DeriveFromKeyPath(addrmgrNs, path)
		if err != nil {
			return err
		}

		keyDesc.KeyLocator = keyLoc
		keyDesc.PubKey = addr.(waddrmgr.ManagedPubKeyAddress).PubKey()

		return nil
	})
	if err != nil {
		return keyDesc, err
	}

	return keyDesc, nil
}

// DerivePrivKey attempts to derive the private key that corresponds to the
// passed key descriptor.
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *BtcWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (*btcec.PrivateKey, error) {
	var key *btcec.PrivateKey

	db := b.wallet.Database()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		scope, err := b.keyScope()
		if err != nil {
			return err
		}

		// If the account doesn't exist, then we may need to create it
		// for the first time in order to derive the keys that we
		// require.
		err = b.createAccountIfNotExists(
			addrmgrNs, keyDesc.Family, scope,
		)
		if err != nil {
			return err
		}

		// Now that we know the account exists, we can safely derive
		// the full private key from the given path.
		path := waddrmgr.DerivationPath{
			Account: uint32(keyDesc.Family),
			Branch:  0,
			Index:   uint32(keyDesc.Index),
		}
		addr, err := scope.DeriveFromKeyPath(addrmgrNs, path)
		if err != nil {
			return err
		}

		key, err = addr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return key, nil
}

// ScalarMult performs a scalar multiplication (ECDH-like operation) between
// the target key descriptor and remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//  sx := k*P s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *BtcWalletKeyRing) ScalarMult(keyDesc KeyDescriptor,
	pub *btcec.PublicKey) ([]byte, error) {

	privKey, err := b.DerivePrivKey(keyDesc)
	if err != nil {
		return nil, err
	}

	s := &btcec.PublicKey{}
	x, y := btcec.S256().ScalarMult(pub.X, pub.Y, privKey.D.Bytes())
	s.X = x
	s.Y = y

	h := sha256.Sum256(s.SerializeCompressed())

	return h[:], nil
}
