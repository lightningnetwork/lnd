package keychain

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightninglabs/neutrino/cache/lru"
)

const (
	// CoinTypeBitcoin specifies the BIP44 coin type for Bitcoin key
	// derivation.
	CoinTypeBitcoin uint32 = 0

	// CoinTypeTestnet specifies the BIP44 coin type for all testnet key
	// derivation.
	CoinTypeTestnet = 1

	// ecdhPrivKeyCacheSize bounds the number of derived ECDH private keys
	// that we'll keep memoized at any one time. In practice ECDH is only
	// invoked against a handful of our own keys (one per key family used
	// for onion decryption), so this size is well above the expected
	// working set.
	ecdhPrivKeyCacheSize = 1000
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
	wallet wallet.Interface

	// chainKeyScope defines the purpose and coin type to be used when generating
	// keys for this keyring.
	chainKeyScope waddrmgr.KeyScope

	// lightningScope is a pointer to the scope that we'll be using as a
	// sub key manager to derive all the keys that we require.
	lightningScope *waddrmgr.ScopedKeyManager

	// ecdhPrivKeyCache memoizes the private keys derived for ECDH so that
	// each subsequent ECDH call against the same key avoids the read-write
	// wallet DB transaction (and the bbolt fdatasync that transaction
	// forces). The cache is keyed by the compressed-serialized public key
	// and bounded by ecdhPrivKeyCacheSize via an LRU eviction policy.
	ecdhPrivKeyCache *lru.Cache[
		[btcec.PubKeyBytesLenCompressed]byte, *cachedPrivKey,
	]
}

// cachedPrivKey stores a btcec.PrivateKey by value so it can be stored in the
// LRU cache, which requires values to implement the cache.Value interface.
type cachedPrivKey struct {
	key btcec.PrivateKey
}

// Size returns the "size" of an entry. We return 1 so that the LRU cache
// bounds the total number of entries rather than doing byte-accurate
// accounting.
func (c *cachedPrivKey) Size() (uint64, error) {
	return 1, nil
}

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The passed waddrmgr.Manager MUST be unlocked in order for the keychain
// to function.
func NewBtcWalletKeyRing(w wallet.Interface, coinType uint32) SecretKeyRing {
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
		ecdhPrivKeyCache: lru.NewCache(
			ecdhPrivKeyCacheSize,
			// Wipe the cache's copy of the private key on
			// eviction. Note this only scrubs the copy held here,
			// not the copies handed back to callers; fully zeroing
			// those (and btcwallet's upstream key cache) is left to
			// a follow-up. See TODO below.
			lru.WithDeleteCallback(
				func(_ [btcec.PubKeyBytesLenCompressed]byte,
					v *cachedPrivKey) {

					v.key.Zero()
				},
			),
		),
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
	if !b.wallet.AddrManager().WatchOnly() &&
		b.wallet.AddrManager().IsLocked() {

		return nil, fmt.Errorf("cannot create BtcWalletKeyRing with " +
			"locked waddrmgr.Manager")
	}

	// If the manager is indeed unlocked, then we'll fetch the scope, cache
	// it, and return to the caller.
	lnScope, err := b.wallet.AddrManager().FetchScopedKeyManager(
		b.chainKeyScope,
	)
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
		// require. We skip this if we're using a remote signer in which
		// case we _need_ to create all accounts when creating the
		// wallet, so it must exist now.
		if !b.wallet.AddrManager().WatchOnly() {
			err = b.createAccountIfNotExists(
				addrmgrNs, keyLoc.Family, scope,
			)
			if err != nil {
				return err
			}
		}

		path := waddrmgr.DerivationPath{
			InternalAccount: uint32(keyLoc.Family),
			Branch:          0,
			Index:           keyLoc.Index,
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
func (b *BtcWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (
	*btcec.PrivateKey, error) {

	var key *btcec.PrivateKey

	scope, err := b.keyScope()
	if err != nil {
		return nil, err
	}

	// First, attempt to see if we can read the key directly from
	// btcwallet's internal cache, if we can then we can skip all the
	// operations below (fast path).
	//
	// TODO(erickcestari): the PubKey == nil guard exists because when a
	// descriptor carries a non-nil PubKey we can't tell whether
	// KeyLocator.Index was set explicitly to 0 or just left at the Go
	// zero value. The PubKey-set branch below defensively scans up to
	// MaxKeyRangeScan derivations for a PubKey match rather than trusting
	// the path, so the fast DeriveFromKeyPathCache path is also gated on
	// PubKey == nil. As a result, hot callers like the node identity key
	// (Family=NodeKey, Index=0, PubKey populated by DeriveKey) always
	// miss this cache and take the read-write wallet DB transaction path
	// on every call. Once we have a way to mark a descriptor's path as
	// authoritative (or btcwallet exposes a key-cache lookup by PubKey),
	// the per-ECDH cache in derivePrivKeyForECDH below can be dropped in
	// favor of this path.
	if keyDesc.PubKey == nil {
		keyPath := waddrmgr.DerivationPath{
			InternalAccount: uint32(keyDesc.Family),
			Account:         uint32(keyDesc.Family),
			Branch:          0,
			Index:           keyDesc.Index,
		}
		privKey, err := scope.DeriveFromKeyPathCache(keyPath)
		if err == nil {
			return privKey, nil
		}
	}

	db := b.wallet.Database()
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		// If the account doesn't exist, then we may need to create it
		// for the first time in order to derive the keys that we
		// require. We skip this if we're using a remote signer in which
		// case we _need_ to create all accounts when creating the
		// wallet, so it must exist now.
		if !b.wallet.AddrManager().WatchOnly() {
			err = b.createAccountIfNotExists(
				addrmgrNs, keyDesc.Family, scope,
			)
			if err != nil {
				return err
			}
		}

		// If the public key isn't set or they have a non-zero index,
		// then we know that the caller instead knows the derivation
		// path for a key.
		if keyDesc.PubKey == nil || keyDesc.Index > 0 {
			// Now that we know the account exists, we can safely
			// derive the full private key from the given path.
			path := waddrmgr.DerivationPath{
				InternalAccount: uint32(keyDesc.Family),
				Branch:          0,
				Index:           keyDesc.Index,
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
		}

		// If the public key isn't nil, then this indicates that we
		// need to scan for the private key, assuming that we know the
		// valid key family.
		nextPath := waddrmgr.DerivationPath{
			InternalAccount: uint32(keyDesc.Family),
			Branch:          0,
			Index:           0,
		}

		// We'll now iterate through our key range in an attempt to
		// find the target public key.
		//
		// TODO(roasbeef): possibly move scanning into wallet to allow
		// to be parallelized
		for i := 0; i < MaxKeyRangeScan; i++ {
			// Derive the next key in the range and fetch its
			// managed address.
			addr, err := scope.DeriveFromKeyPath(
				addrmgrNs, nextPath,
			)
			if err != nil {
				return err
			}
			managedAddr := addr.(waddrmgr.ManagedPubKeyAddress)

			// If this is the target public key, then we'll return
			// it directly back to the caller.
			if managedAddr.PubKey().IsEqual(keyDesc.PubKey) {
				key, err = managedAddr.PrivKey()
				if err != nil {
					return err
				}

				return nil
			}

			// This wasn't the target key, so roll forward and try
			// the next one.
			nextPath.Index++
		}

		// If we reach this point, then we we're unable to derive the
		// private key, so return an error back to the user.
		return ErrCannotDerivePrivKey
	})
	if err != nil {
		return nil, err
	}

	return key, nil
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// target key descriptor and remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//	sx := k*P s := sha256(sx.SerializeCompressed())
//
// The derived private key for keyDesc is memoized after the first call so
// repeated ECDH operations against the same key avoid reopening a read-write
// wallet DB transaction (and the bbolt fdatasync per call) on the hot path.
//
// NOTE: This is part of the keychain.ECDHRing interface.
func (b *BtcWalletKeyRing) ECDH(keyDesc KeyDescriptor,
	pub *btcec.PublicKey) ([32]byte, error) {

	privKey, err := b.derivePrivKeyForECDH(keyDesc)
	if err != nil {
		return [32]byte{}, err
	}

	var (
		pubJacobian btcec.JacobianPoint
		s           btcec.JacobianPoint
	)
	pub.AsJacobian(&pubJacobian)

	btcec.ScalarMultNonConst(&privKey.Key, &pubJacobian, &s)
	s.ToAffine()
	sPubKey := btcec.NewPublicKey(&s.X, &s.Y)
	h := sha256.Sum256(sPubKey.SerializeCompressed())

	return h, nil
}

// derivePrivKeyForECDH returns the private key for keyDesc, consulting and
// populating the per-keyring ECDH cache. The cache is keyed by the
// compressed-serialized public key, which uniquely identifies the private key
// regardless of whether DerivePrivKey takes the path-based or the PubKey-scan
// branch. When the descriptor has no public key set we cannot cache (we have
// nothing collision-free to key on without first deriving) and forward to
// DerivePrivKey directly. Production ECDH callers always supply a PubKey, so
// the no-PubKey path is not on the hot path.
//
// NOTE: we assume the KeyDescriptor passed here is always valid, in the sense
// that its KeyLocator and PubKey are consistent (i.e. the key derived from the
// KeyLocator has the public key in PubKey). This lets us cache the derived key
// without re-checking priv.PubKey().IsEqual(keyDesc.PubKey) on every call. Such
// an inconsistency should never happen, but the assumption is called out
// explicitly given this is a critical area.
//
// TODO(erickcestari): this cache only exists because btcwallet's internal
// DeriveFromKeyPathCache is skipped when KeyDescriptor.PubKey is set
// (see DerivePrivKey above). The hot ECDH callers (node identity key,
// per-channel revocation/base-encryption keys, signrpc descriptors)
// always supply a PubKey, so they never hit btcwallet's cache and
// each call opens a read-write wallet DB transaction. When btcwallet
// gains a cache lookup path that handles the PubKey-populated
// descriptor case (or unambiguously distinguishes an explicit Index=0
// from the Go zero value), this whole helper and the ecdhPrivKeyCache
// field on BtcWalletKeyRing should be removed and ECDH can call
// DerivePrivKey directly.
func (b *BtcWalletKeyRing) derivePrivKeyForECDH(keyDesc KeyDescriptor) (
	*btcec.PrivateKey, error) {

	if keyDesc.PubKey == nil {
		return b.DerivePrivKey(keyDesc)
	}

	var cacheKey [btcec.PubKeyBytesLenCompressed]byte
	copy(cacheKey[:], keyDesc.PubKey.SerializeCompressed())

	if v, err := b.ecdhPrivKeyCache.Get(cacheKey); err == nil {
		privKey := v.key
		return &privKey, nil
	} else if !errors.Is(err, cache.ErrElementNotFound) {
		return nil, err
	}

	priv, err := b.DerivePrivKey(keyDesc)
	if err != nil {
		return nil, err
	}

	// A Put failure here is not expected: each entry has size 1, so it
	// can never exceed the cache capacity. If it ever does, surface it
	// rather than silently swallowing it, since it means an invariant we
	// rely on has been broken.
	if _, err := b.ecdhPrivKeyCache.Put(
		cacheKey, &cachedPrivKey{key: *priv},
	); err != nil {
		return nil, err
	}

	return priv, nil
}

// SignMessage signs the given message, single or double SHA256 hashing it
// first, with the private key described in the key locator.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessage(keyLoc KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return ecdsa.Sign(privKey, digest), nil
}

// SignMessageCompact signs the given message, single or double SHA256 hashing
// it first, with the private key described in the key locator and returns
// the signature in the compact, public key recoverable format.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessageCompact(keyLoc KeyLocator,
	msg []byte, doubleHash bool) ([]byte, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}

	return ecdsa.SignCompact(privKey, digest, true), nil
}

// SignMessageSchnorr uses the Schnorr signature algorithm to sign the given
// message, single or double SHA256 hashing it first, with the private key
// described in the key locator and the optional tweak applied to the private
// key.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessageSchnorr(keyLoc KeyLocator,
	msg []byte, doubleHash bool, taprootTweak []byte,
	tag []byte) (*schnorr.Signature, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	if len(taprootTweak) > 0 {
		privKey = txscript.TweakTaprootPrivKey(*privKey, taprootTweak)
	}

	// If a tag was provided, we need to take the tagged hash of the input.
	var digest []byte
	switch {
	case len(tag) > 0:
		taggedHash := chainhash.TaggedHash(tag, msg)
		digest = taggedHash[:]
	case doubleHash:
		digest = chainhash.DoubleHashB(msg)
	default:
		digest = chainhash.HashB(msg)
	}
	return schnorr.Sign(privKey, digest)
}
