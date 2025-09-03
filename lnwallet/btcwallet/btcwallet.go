package btcwallet

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	defaultAccount  = uint32(waddrmgr.DefaultAccountNum)
	importedAccount = uint32(waddrmgr.ImportedAddrAccount)

	// dryRunImportAccountNumAddrs represents the number of addresses we'll
	// derive for an imported account's external and internal branch when a
	// dry run is attempted.
	dryRunImportAccountNumAddrs = 5

	// UnconfirmedHeight is the special case end height that is used to
	// obtain unconfirmed transactions from ListTransactionDetails.
	UnconfirmedHeight int32 = -1

	// walletMetaBucket is used to store wallet metadata.
	walletMetaBucket = "lnwallet"

	// walletReadyKey is used to indicate that the wallet has been
	// initialized.
	walletReadyKey = "ready"
)

var (
	// lightningAddrSchema is the scope addr schema for all keys that we
	// derive. We'll treat them all as p2wkh addresses, as atm we must
	// specify a particular type.
	lightningAddrSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}

	// LndDefaultKeyScopes is the list of default key scopes that lnd adds
	// to its wallet.
	LndDefaultKeyScopes = []waddrmgr.KeyScope{
		waddrmgr.KeyScopeBIP0049Plus,
		waddrmgr.KeyScopeBIP0084,
		waddrmgr.KeyScopeBIP0086,
	}

	// errNoImportedAddrGen is an error returned when a new address is
	// requested for the default imported account within the wallet.
	errNoImportedAddrGen = errors.New("addresses cannot be generated for " +
		"the default imported account")
)

// BtcWallet is an implementation of the lnwallet.WalletController interface
// backed by an active instance of btcwallet. At the time of the writing of
// this documentation, this implementation requires a full btcd node to
// operate.
type BtcWallet struct {
	// wallet is an active instance of btcwallet.
	wallet base.Interface

	chain chain.Interface

	db walletdb.DB

	cfg *Config

	netParams *chaincfg.Params

	chainKeyScope waddrmgr.KeyScope

	blockCache *blockcache.BlockCache

	*input.MusigSessionManager
}

// A compile time check to ensure that BtcWallet implements the
// WalletController and BlockChainIO interfaces.
var _ lnwallet.WalletController = (*BtcWallet)(nil)
var _ lnwallet.BlockChainIO = (*BtcWallet)(nil)

// New returns a new fully initialized instance of BtcWallet given a valid
// configuration struct.
func New(cfg Config, blockCache *blockcache.BlockCache) (*BtcWallet, error) {
	// Create the key scope for the coin type being managed by this wallet.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    cfg.CoinType,
	}

	// Maybe the wallet has already been opened and unlocked by the
	// WalletUnlocker. So if we get a non-nil value from the config,
	// we assume everything is in order.
	var wallet = cfg.Wallet
	if wallet == nil {
		// No ready wallet was passed, so try to open an existing one.
		var pubPass []byte
		if cfg.PublicPass == nil {
			pubPass = defaultPubPassphrase
		} else {
			pubPass = cfg.PublicPass
		}

		loader, err := NewWalletLoader(
			cfg.NetParams, cfg.RecoveryWindow, cfg.LoaderOptions...,
		)
		if err != nil {
			return nil, err
		}
		walletExists, err := loader.WalletExists()
		if err != nil {
			return nil, err
		}

		if !walletExists {
			// Wallet has never been created, perform initial
			// set up.
			wallet, err = loader.CreateNewWallet(
				pubPass, cfg.PrivatePass, cfg.HdSeed,
				cfg.Birthday,
			)
			if err != nil {
				return nil, err
			}
		} else {
			// Wallet has been created and been initialized at
			// this point, open it along with all the required DB
			// namespaces, and the DB itself.
			wallet, err = loader.OpenExistingWallet(pubPass, false)
			if err != nil {
				return nil, err
			}
		}
	}

	finalWallet := &BtcWallet{
		cfg:           &cfg,
		wallet:        wallet,
		db:            wallet.Database(),
		chain:         cfg.ChainSource,
		netParams:     cfg.NetParams,
		chainKeyScope: chainKeyScope,
		blockCache:    blockCache,
	}

	finalWallet.MusigSessionManager = input.NewMusigSessionManager(
		finalWallet.fetchPrivKey,
	)

	return finalWallet, nil
}

// loaderCfg holds optional wallet loader configuration.
type loaderCfg struct {
	dbDirPath      string
	noFreelistSync bool
	dbTimeout      time.Duration
	useLocalDB     bool
	externalDB     kvdb.Backend
}

// LoaderOption is a functional option to update the optional loader config.
type LoaderOption func(*loaderCfg)

// LoaderWithLocalWalletDB configures the wallet loader to use the local db.
func LoaderWithLocalWalletDB(dbDirPath string, noFreelistSync bool,
	dbTimeout time.Duration) LoaderOption {

	return func(cfg *loaderCfg) {
		cfg.dbDirPath = dbDirPath
		cfg.noFreelistSync = noFreelistSync
		cfg.dbTimeout = dbTimeout
		cfg.useLocalDB = true
	}
}

// LoaderWithExternalWalletDB configures the wallet loadr to use an external db.
func LoaderWithExternalWalletDB(db kvdb.Backend) LoaderOption {
	return func(cfg *loaderCfg) {
		cfg.externalDB = db
	}
}

// NewWalletLoader constructs a wallet loader.
func NewWalletLoader(chainParams *chaincfg.Params, recoveryWindow uint32,
	opts ...LoaderOption) (*base.Loader, error) {

	cfg := &loaderCfg{}

	// Apply all functional options.
	for _, o := range opts {
		o(cfg)
	}

	if cfg.externalDB != nil && cfg.useLocalDB {
		return nil, fmt.Errorf("wallet can either be in the local or " +
			"an external db")
	}

	if cfg.externalDB != nil {
		loader, err := base.NewLoaderWithDB(
			chainParams, recoveryWindow, cfg.externalDB,
			func() (bool, error) {
				return externalWalletExists(cfg.externalDB)
			},
		)
		if err != nil {
			return nil, err
		}

		// Decorate wallet db with out own key such that we
		// can always check whether the wallet exists or not.
		loader.OnWalletCreated(onWalletCreated)
		return loader, nil
	}

	return base.NewLoader(
		chainParams, cfg.dbDirPath, cfg.noFreelistSync,
		cfg.dbTimeout, recoveryWindow,
	), nil
}

// externalWalletExists is a helper function that we use to template btcwallet's
// Loader in order to be able check if the wallet database has been initialized
// in an external DB.
func externalWalletExists(db kvdb.Backend) (bool, error) {
	exists := false
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		metaBucket := tx.ReadBucket([]byte(walletMetaBucket))
		if metaBucket != nil {
			walletReady := metaBucket.Get([]byte(walletReadyKey))
			exists = string(walletReady) == walletReadyKey
		}

		return nil
	}, func() {})

	return exists, err
}

// onWalletCreated is executed when btcwallet creates the wallet the first time.
func onWalletCreated(tx kvdb.RwTx) error {
	metaBucket, err := tx.CreateTopLevelBucket([]byte(walletMetaBucket))
	if err != nil {
		return err
	}

	return metaBucket.Put([]byte(walletReadyKey), []byte(walletReadyKey))
}

// BackEnd returns the underlying ChainService's name as a string.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) BackEnd() string {
	if b.chain != nil {
		return b.chain.BackEnd()
	}

	return ""
}

// InternalWallet returns a pointer to the internal base wallet which is the
// core of btcwallet.
func (b *BtcWallet) InternalWallet() base.Interface {
	return b.wallet
}

// Start initializes the underlying rpc connection, the wallet itself, and
// begins syncing to the current available blockchain state.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Start() error {
	// Is the wallet (according to its database) currently watch-only
	// already? If it is, we won't need to convert it later.
	walletIsWatchOnly := b.wallet.AddrManager().WatchOnly()

	// If the wallet is watch-only, but we don't expect it to be, then we
	// are in an unexpected state and cannot continue.
	if walletIsWatchOnly && !b.cfg.WatchOnly {
		return fmt.Errorf("wallet is watch-only but we expect it " +
			"not to be; check if remote signing was disabled by " +
			"accident")
	}

	// We'll start by unlocking the wallet and ensuring that the KeyScope:
	// (1017, 1) exists within the internal waddrmgr. We'll need this in
	// order to properly generate the keys required for signing various
	// contracts. If this is a watch-only wallet, we don't have any private
	// keys and therefore unlocking is not necessary.
	if !walletIsWatchOnly {
		if err := b.wallet.Unlock(b.cfg.PrivatePass, nil); err != nil {
			return err
		}

		// If the wallet isn't about to be converted, we need to inform
		// the user that this wallet still contains all private key
		// material and that they need to migrate the existing wallet.
		if b.cfg.WatchOnly && !b.cfg.MigrateWatchOnly {
			log.Warnf("Wallet is expected to be in watch-only " +
				"mode but hasn't been migrated to watch-only " +
				"yet, it still contains private keys; " +
				"consider turning on the watch-only wallet " +
				"migration in remote signing mode")
		}
	}

	// Because we might add new "default" key scopes over time, they are
	// created correctly for new wallets. Existing wallets don't
	// automatically add them, we need to do that manually now.
	for _, scope := range LndDefaultKeyScopes {
		_, err := b.wallet.AddrManager().FetchScopedKeyManager(scope)
		if waddrmgr.IsError(err, waddrmgr.ErrScopeNotFound) {
			// The default scope wasn't found, that probably means
			// it was added recently and older wallets don't know it
			// yet. Let's add it now.
			addrSchema := waddrmgr.ScopeAddrMap[scope]
			_, err := b.wallet.AddScopeManager(scope, addrSchema)
			if err != nil {
				return err
			}
		}
	}

	scope, err := b.wallet.AddrManager().FetchScopedKeyManager(
		b.chainKeyScope,
	)
	if err != nil {
		// If the scope hasn't yet been created (it wouldn't been
		// loaded by default if it was), then we'll manually create the
		// scope for the first time ourselves.
		manager, err := b.wallet.AddScopeManager(
			b.chainKeyScope, lightningAddrSchema,
		)
		if err != nil {
			return err
		}

		scope = manager
	}

	// If the wallet is not watch-only atm, and the user wants to migrate it
	// to watch-only, we will set `convertToWatchOnly` to true so the wallet
	// accounts are created and converted.
	convertToWatchOnly := !walletIsWatchOnly && b.cfg.WatchOnly &&
		b.cfg.MigrateWatchOnly

	// Now that the wallet is unlocked, we'll go ahead and make sure we
	// create accounts for all the key families we're going to use. This
	// will make it possible to list all the account/family xpubs in the
	// wallet list RPC.
	err = b.wallet.InitAccounts(scope, convertToWatchOnly, 255)
	if err != nil {
		return err
	}

	// Establish an RPC connection in addition to starting the goroutines
	// in the underlying wallet.
	if err := b.chain.Start(); err != nil {
		return err
	}

	// Start the underlying btcwallet core.
	b.wallet.Start()

	// Pass the rpc client into the wallet so it can sync up to the
	// current main chain.
	b.wallet.SynchronizeRPC(b.chain)

	return nil
}

// Stop signals the wallet for shutdown. Shutdown may entail closing
// any active sockets, database handles, stopping goroutines, etc.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Stop() error {
	b.wallet.Stop()

	b.wallet.WaitForShutdown()

	b.chain.Stop()

	return nil
}

// ConfirmedBalance returns the sum of all the wallet's unspent outputs that
// have at least confs confirmations. If confs is set to zero, then all unspent
// outputs, including those currently in the mempool will be included in the
// final sum. The account parameter serves as a filter to retrieve the balance
// for a specific account. When empty, the confirmed balance of all wallet
// accounts is returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ConfirmedBalance(confs int32,
	accountFilter string) (btcutil.Amount, error) {

	var balance btcutil.Amount

	witnessOutputs, err := b.ListUnspentWitness(
		confs, math.MaxInt32, accountFilter,
	)
	if err != nil {
		return 0, err
	}

	for _, witnessOutput := range witnessOutputs {
		balance += witnessOutput.Value
	}

	return balance, nil
}

// keyScopeForAccountAddr determines the appropriate key scope of an account
// based on its name/address type.
func (b *BtcWallet) keyScopeForAccountAddr(accountName string,
	addrType lnwallet.AddressType) (waddrmgr.KeyScope, uint32, error) {

	// Map the requested address type to its key scope.
	var addrKeyScope waddrmgr.KeyScope
	switch addrType {
	case lnwallet.WitnessPubKey:
		addrKeyScope = waddrmgr.KeyScopeBIP0084
	case lnwallet.NestedWitnessPubKey:
		addrKeyScope = waddrmgr.KeyScopeBIP0049Plus
	case lnwallet.TaprootPubkey:
		addrKeyScope = waddrmgr.KeyScopeBIP0086
	default:
		return waddrmgr.KeyScope{}, 0,
			fmt.Errorf("unknown address type")
	}

	// The default account spans across multiple key scopes, so the
	// requested address type should already be valid for this account.
	if accountName == lnwallet.DefaultAccountName {
		return addrKeyScope, defaultAccount, nil
	}

	// Otherwise, look up the custom account and if it supports the given
	// key scope.
	accountNumber, err := b.wallet.AccountNumber(addrKeyScope, accountName)
	if err != nil {
		return waddrmgr.KeyScope{}, 0, err
	}

	return addrKeyScope, accountNumber, nil
}

// NewAddress returns the next external or internal address for the wallet
// dictated by the value of the `change` parameter. If change is true, then an
// internal address will be returned, otherwise an external address should be
// returned. The account parameter must be non-empty as it determines which
// account the address should be generated from.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) NewAddress(t lnwallet.AddressType, change bool,
	accountName string) (btcutil.Address, error) {

	// Addresses cannot be derived from the catch-all imported accounts.
	if accountName == waddrmgr.ImportedAddrAccountName {
		return nil, errNoImportedAddrGen
	}

	keyScope, account, err := b.keyScopeForAccountAddr(accountName, t)
	if err != nil {
		return nil, err
	}

	if change {
		return b.wallet.NewChangeAddress(account, keyScope)
	}
	return b.wallet.NewAddress(account, keyScope)
}

// LastUnusedAddress returns the last *unused* address known by the wallet. An
// address is unused if it hasn't received any payments. This can be useful in
// UIs in order to continually show the "freshest" address without having to
// worry about "address inflation" caused by continual refreshing. Similar to
// NewAddress it can derive a specified address type, and also optionally a
// change address. The account parameter must be non-empty as it determines
// which account the address should be generated from.
func (b *BtcWallet) LastUnusedAddress(addrType lnwallet.AddressType,
	accountName string) (btcutil.Address, error) {

	// Addresses cannot be derived from the catch-all imported accounts.
	if accountName == waddrmgr.ImportedAddrAccountName {
		return nil, errNoImportedAddrGen
	}

	keyScope, account, err := b.keyScopeForAccountAddr(accountName, addrType)
	if err != nil {
		return nil, err
	}

	return b.wallet.CurrentAddress(account, keyScope)
}

// IsOurAddress checks if the passed address belongs to this wallet
//
// This is a part of the WalletController interface.
func (b *BtcWallet) IsOurAddress(a btcutil.Address) bool {
	result, err := b.wallet.HaveAddress(a)
	return result && (err == nil)
}

// AddressInfo returns the information about an address, if it's known to this
// wallet.
//
// NOTE: This is a part of the WalletController interface.
func (b *BtcWallet) AddressInfo(a btcutil.Address) (waddrmgr.ManagedAddress,
	error) {

	return b.wallet.AddressInfo(a)
}

// ListAccounts retrieves all accounts belonging to the wallet by default. A
// name and key scope filter can be provided to filter through all of the wallet
// accounts and return only those matching.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListAccounts(name string,
	keyScope *waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties, error) {

	var res []*waddrmgr.AccountProperties
	switch {
	// If both the name and key scope filters were provided, we'll return
	// the existing account matching those.
	case name != "" && keyScope != nil:
		account, err := b.wallet.AccountPropertiesByName(*keyScope, name)
		if err != nil {
			return nil, err
		}
		res = append(res, account)

	// Only the name filter was provided.
	case name != "" && keyScope == nil:
		// If the name corresponds to the default or imported accounts,
		// we'll return them for all our supported key scopes.
		if name == lnwallet.DefaultAccountName ||
			name == waddrmgr.ImportedAddrAccountName {

			for _, defaultScope := range LndDefaultKeyScopes {
				a, err := b.wallet.AccountPropertiesByName(
					defaultScope, name,
				)
				if err != nil {
					return nil, err
				}
				res = append(res, a)
			}

			break
		}

		// In theory, there should be only one custom account for the
		// given name. However, due to a lack of check, users could
		// create custom accounts with various key scopes. This
		// behaviour has been fixed but, we return all potential custom
		// accounts with the given name.
		for _, scope := range waddrmgr.DefaultKeyScopes {
			a, err := b.wallet.AccountPropertiesByName(
				scope, name,
			)
			switch {
			case waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound):
				continue

			// In the specific case of a wallet initialized only by
			// importing account xpubs (watch only wallets), it is
			// possible that some keyscopes will be 'unknown' by the
			// wallet (depending on the xpubs given to initialize
			// it). If the keyscope is not found, just skip it.
			case waddrmgr.IsError(err, waddrmgr.ErrScopeNotFound):
				continue

			case err != nil:
				return nil, err
			}

			res = append(res, a)
		}
		if len(res) == 0 {
			return nil, newAccountNotFoundError(name)
		}

	// Only the key scope filter was provided, so we'll return all accounts
	// matching it.
	case name == "" && keyScope != nil:
		accounts, err := b.wallet.Accounts(*keyScope)
		if err != nil {
			return nil, err
		}
		for _, account := range accounts.Accounts {
			account := account
			res = append(res, &account.AccountProperties)
		}

	// Neither of the filters were provided, so return all accounts for our
	// supported key scopes.
	case name == "" && keyScope == nil:
		for _, defaultScope := range LndDefaultKeyScopes {
			accounts, err := b.wallet.Accounts(defaultScope)
			if err != nil {
				return nil, err
			}
			for _, account := range accounts.Accounts {
				account := account
				res = append(res, &account.AccountProperties)
			}
		}

		accounts, err := b.wallet.Accounts(waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose,
			Coin:    b.cfg.CoinType,
		})
		if err != nil {
			return nil, err
		}
		for _, account := range accounts.Accounts {
			account := account
			res = append(res, &account.AccountProperties)
		}
	}

	return res, nil
}

// newAccountNotFoundError returns an error indicating that the manager didn't
// find the specific account. This error is used to be compatible with the old
// 'LookupAccount' behaviour previously used.
func newAccountNotFoundError(name string) error {
	str := fmt.Sprintf("account name '%s' not found", name)

	return waddrmgr.ManagerError{
		ErrorCode:   waddrmgr.ErrAccountNotFound,
		Description: str,
	}
}

// RequiredReserve returns the minimum amount of satoshis that should be
// kept in the wallet in order to fee bump anchor channels if necessary.
// The value scales with the number of public anchor channels but is
// capped at a maximum.
func (b *BtcWallet) RequiredReserve(
	numAnchorChans uint32) btcutil.Amount {

	anchorChanReservedValue := lnwallet.AnchorChanReservedValue
	reserved := btcutil.Amount(numAnchorChans) * anchorChanReservedValue
	if reserved > lnwallet.MaxAnchorChanReservedValue {
		reserved = lnwallet.MaxAnchorChanReservedValue
	}

	return reserved
}

// ListAddresses retrieves all the addresses along with their balance. An
// account name filter can be provided to filter through all of the
// wallet accounts and return the addresses of only those matching.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListAddresses(name string,
	showCustomAccounts bool) (lnwallet.AccountAddressMap, error) {

	accounts, err := b.ListAccounts(name, nil)
	if err != nil {
		return nil, err
	}

	addresses := make(lnwallet.AccountAddressMap)
	addressBalance := make(map[string]btcutil.Amount)

	// Retrieve all the unspent ouputs.
	outputs, err := b.wallet.ListUnspent(0, math.MaxInt32, "")
	if err != nil {
		return nil, err
	}

	// Calculate the total balance of each address.
	for _, output := range outputs {
		amount, err := btcutil.NewAmount(output.Amount)
		if err != nil {
			return nil, err
		}

		addressBalance[output.Address] += amount
	}

	for _, accntDetails := range accounts {
		accntScope := accntDetails.KeyScope
		managedAddrs, err := b.wallet.AccountManagedAddresses(
			accntDetails.KeyScope, accntDetails.AccountNumber,
		)
		if err != nil {
			return nil, err
		}

		// Only consider those accounts which have addresses.
		if len(managedAddrs) == 0 {
			continue
		}

		// All the lnd internal/custom keys for channels and other
		// functionality are derived from the same scope. Since they
		// aren't really used as addresses and will never have an
		// on-chain balance, we'll want to show the public key instead.
		isLndCustom := accntScope.Purpose == keychain.BIP0043Purpose
		addressProperties := make(
			[]lnwallet.AddressProperty, len(managedAddrs),
		)

		for idx, managedAddr := range managedAddrs {
			addr := managedAddr.Address()
			addressString := addr.String()

			// Hex-encode the compressed public key for custom lnd
			// keys, addresses don't make a lot of sense.
			var (
				pubKey         *btcec.PublicKey
				derivationPath string
			)
			pka, ok := managedAddr.(waddrmgr.ManagedPubKeyAddress)
			if ok {
				pubKey = pka.PubKey()

				// There can be an error in two cases: Either
				// the address isn't a managed pubkey address,
				// which we already checked above, or the
				// address is imported in which case we don't
				// know the derivation path, and it will just be
				// empty anyway.
				_, _, derivationPath, _ =
					Bip32DerivationFromAddress(pka)
			}
			if pubKey != nil && isLndCustom {
				addressString = hex.EncodeToString(
					pubKey.SerializeCompressed(),
				)
			}

			addressProperties[idx] = lnwallet.AddressProperty{
				Address:        addressString,
				Internal:       managedAddr.Internal(),
				Balance:        addressBalance[addressString],
				PublicKey:      pubKey,
				DerivationPath: derivationPath,
			}
		}

		if accntScope.Purpose != keychain.BIP0043Purpose ||
			showCustomAccounts {

			addresses[accntDetails] = addressProperties
		}
	}

	return addresses, nil
}

// ImportAccount imports an account backed by an account extended public key.
// The master key fingerprint denotes the fingerprint of the root key
// corresponding to the account public key (also known as the key with
// derivation path m/). This may be required by some hardware wallets for proper
// identification and signing.
//
// The address type can usually be inferred from the key's version, but may be
// required for certain keys to map them into the proper scope.
//
// For custom accounts, we will first check if there is no account with the same
// name (even with a different key scope). No custom account should have various
// key scopes as it will result in non-deterministic behaviour.
//
// For BIP-0044 keys, an address type must be specified as we intend to not
// support importing BIP-0044 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will force
// the standard BIP-0049 derivation scheme, while a witness address type will
// force the standard BIP-0084 derivation scheme.
//
// For BIP-0049 keys, an address type must also be specified to make a
// distinction between the standard BIP-0049 address schema (nested witness
// pubkeys everywhere) and our own BIP-0049Plus address schema (nested pubkeys
// externally, witness pubkeys internally).
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ImportAccount(name string, accountPubKey *hdkeychain.ExtendedKey,
	masterKeyFingerprint uint32, addrType *waddrmgr.AddressType,
	dryRun bool) (*waddrmgr.AccountProperties, []btcutil.Address,
	[]btcutil.Address, error) {

	// For custom accounts, we first check if there is no existing account
	// with the same name.
	if name != lnwallet.DefaultAccountName &&
		name != waddrmgr.ImportedAddrAccountName {

		_, err := b.ListAccounts(name, nil)
		if err == nil {
			return nil, nil, nil,
				fmt.Errorf("account '%s' already exists",
					name)
		}
		if !waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound) {
			return nil, nil, nil, err
		}
	}

	if !dryRun {
		accountProps, err := b.wallet.ImportAccount(
			name, accountPubKey, masterKeyFingerprint, addrType,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		return accountProps, nil, nil, nil
	}

	// Derive addresses from both the external and internal branches of the
	// account. There's no risk of address inflation as this is only done
	// for dry runs.
	accountProps, extAddrs, intAddrs, err := b.wallet.ImportAccountDryRun(
		name, accountPubKey, masterKeyFingerprint, addrType,
		dryRunImportAccountNumAddrs,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	externalAddrs := make([]btcutil.Address, len(extAddrs))
	for i := 0; i < len(extAddrs); i++ {
		externalAddrs[i] = extAddrs[i].Address()
	}

	internalAddrs := make([]btcutil.Address, len(intAddrs))
	for i := 0; i < len(intAddrs); i++ {
		internalAddrs[i] = intAddrs[i].Address()
	}

	return accountProps, externalAddrs, internalAddrs, nil
}

// ImportPublicKey imports a single derived public key into the wallet. The
// address type can usually be inferred from the key's version, but in the case
// of legacy versions (xpub, tpub), an address type must be specified as we
// intend to not support importing BIP-44 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ImportPublicKey(pubKey *btcec.PublicKey,
	addrType waddrmgr.AddressType) error {

	return b.wallet.ImportPublicKey(pubKey, addrType)
}

// ImportTaprootScript imports a user-provided taproot script into the address
// manager. The imported script will act as a pay-to-taproot address.
func (b *BtcWallet) ImportTaprootScript(scope waddrmgr.KeyScope,
	tapscript *waddrmgr.Tapscript) (waddrmgr.ManagedAddress, error) {

	// We want to be able to import script addresses into a watch-only
	// wallet, which is only possible if we don't encrypt the script with
	// the private key encryption key. By specifying the script as being
	// "not secret", we can also decrypt the script in a watch-only wallet.
	const isSecretScript = false

	// Currently, only v1 (Taproot) scripts are supported. We don't even
	// know what a v2 witness version would look like at this point.
	const witnessVersionTaproot byte = 1

	return b.wallet.ImportTaprootScript(
		scope, tapscript, nil, witnessVersionTaproot, isSecretScript,
	)
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SendOutputs(inputs fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight,
	minConfs int32, label string,
	strategy base.CoinSelectionStrategy) (*wire.MsgTx, error) {

	// Convert our fee rate from sat/kw to sat/kb since it's required by
	// SendOutputs.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	// Sanity check outputs.
	if len(outputs) < 1 {
		return nil, lnwallet.ErrNoOutputs
	}

	// Sanity check minConfs.
	if minConfs < 0 {
		return nil, lnwallet.ErrInvalidMinconf
	}

	// Use selected UTXOs if specified, otherwise default selection.
	if len(inputs) != 0 {
		return b.wallet.SendOutputsWithInput(
			outputs, nil, defaultAccount, minConfs, feeSatPerKB,
			strategy, label, inputs.ToSlice(),
		)
	}

	return b.wallet.SendOutputs(
		outputs, nil, defaultAccount, minConfs, feeSatPerKB,
		strategy, label,
	)
}

// CreateSimpleTx creates a Bitcoin transaction paying to the specified
// outputs. The transaction is not broadcasted to the network, but a new change
// address might be created in the wallet database. In the case the wallet has
// insufficient funds, or the outputs are non-standard, an error should be
// returned. This method also takes the target fee expressed in sat/kw that
// should be used when crafting the transaction.
//
// NOTE: The dryRun argument can be set true to create a tx that doesn't alter
// the database. A tx created with this set to true SHOULD NOT be broadcasted.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) CreateSimpleTx(inputs fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight, minConfs int32,
	strategy base.CoinSelectionStrategy, dryRun bool) (
	*txauthor.AuthoredTx, error) {

	// The fee rate is passed in using units of sat/kw, so we'll convert
	// this to sat/KB as the CreateSimpleTx method requires this unit.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	// Sanity check outputs.
	if len(outputs) < 1 {
		return nil, lnwallet.ErrNoOutputs
	}

	// Sanity check minConfs.
	if minConfs < 0 {
		return nil, lnwallet.ErrInvalidMinconf
	}

	for _, output := range outputs {
		// When checking an output for things like dusty-ness, we'll
		// use the default mempool relay fee rather than the target
		// effective fee rate to ensure accuracy. Otherwise, we may
		// mistakenly mark small-ish, but not quite dust output as
		// dust.
		err := txrules.CheckOutput(
			output, txrules.DefaultRelayFeePerKb,
		)
		if err != nil {
			return nil, err
		}
	}

	// Add the optional inputs to the transaction.
	optFunc := base.WithCustomSelectUtxos(inputs.ToSlice())

	return b.wallet.CreateSimpleTx(
		nil, defaultAccount, outputs, minConfs, feeSatPerKB,
		strategy, dryRun, []base.TxCreateOption{optFunc}...,
	)
}

// LeaseOutput locks an output to the given ID, preventing it from being
// available for any future coin selection attempts. The absolute time of the
// lock's expiration is returned. The expiration of the lock can be extended by
// successive invocations of this call. Outputs can be unlocked before their
// expiration through `ReleaseOutput`.
//
// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If the
// output has already been locked to a different ID, then
// wtxmgr.ErrOutputAlreadyLocked is returned.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *BtcWallet) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
	duration time.Duration) (time.Time, error) {

	// Make sure we don't attempt to double lock an output that's been
	// locked by the in-memory implementation.
	if b.wallet.LockedOutpoint(op) {
		return time.Time{}, wtxmgr.ErrOutputAlreadyLocked
	}

	lockedUntil, err := b.wallet.LeaseOutput(id, op, duration)
	if err != nil {
		return time.Time{}, err
	}

	return lockedUntil, nil
}

// ListLeasedOutputs returns a list of all currently locked outputs.
func (b *BtcWallet) ListLeasedOutputs() ([]*base.ListLeasedOutputResult,
	error) {

	return b.wallet.ListLeasedOutputs()
}

// ReleaseOutput unlocks an output, allowing it to be available for coin
// selection if it remains unspent. The ID should match the one used to
// originally lock the output.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *BtcWallet) ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error {
	return b.wallet.ReleaseOutput(id, op)
}

// ListUnspentWitness returns all unspent outputs which are version 0 witness
// programs. The 'minConfs' and 'maxConfs' parameters indicate the minimum
// and maximum number of confirmations an output needs in order to be returned
// by this method. Passing -1 as 'minConfs' indicates that even unconfirmed
// outputs should be returned. Using MaxInt32 as 'maxConfs' implies returning
// all outputs with at least 'minConfs'. The account parameter serves as a
// filter to retrieve the unspent outputs for a specific account.  When empty,
// the unspent outputs of all wallet accounts are returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListUnspentWitness(minConfs, maxConfs int32,
	accountFilter string) ([]*lnwallet.Utxo, error) {

	// First, grab all the unfiltered currently unspent outputs.
	unspentOutputs, err := b.wallet.ListUnspent(
		minConfs, maxConfs, accountFilter,
	)
	if err != nil {
		return nil, err
	}

	// Next, we'll run through all the regular outputs, only saving those
	// which are p2wkh outputs or a p2wsh output nested within a p2sh output.
	witnessOutputs := make([]*lnwallet.Utxo, 0, len(unspentOutputs))
	for _, output := range unspentOutputs {
		pkScript, err := hex.DecodeString(output.ScriptPubKey)
		if err != nil {
			return nil, err
		}

		addressType := lnwallet.UnknownAddressType
		if txscript.IsPayToWitnessPubKeyHash(pkScript) {
			addressType = lnwallet.WitnessPubKey
		} else if txscript.IsPayToScriptHash(pkScript) {
			// TODO(roasbeef): This assumes all p2sh outputs returned by the
			// wallet are nested p2pkh. We can't check the redeem script because
			// the btcwallet service does not include it.
			addressType = lnwallet.NestedWitnessPubKey
		} else if txscript.IsPayToTaproot(pkScript) {
			addressType = lnwallet.TaprootPubkey
		}

		if addressType == lnwallet.WitnessPubKey ||
			addressType == lnwallet.NestedWitnessPubKey ||
			addressType == lnwallet.TaprootPubkey {

			txid, err := chainhash.NewHashFromStr(output.TxID)
			if err != nil {
				return nil, err
			}

			// We'll ensure we properly convert the amount given in
			// BTC to satoshis.
			amt, err := btcutil.NewAmount(output.Amount)
			if err != nil {
				return nil, err
			}

			utxo := &lnwallet.Utxo{
				AddressType: addressType,
				Value:       amt,
				PkScript:    pkScript,
				OutPoint: wire.OutPoint{
					Hash:  *txid,
					Index: output.Vout,
				},
				Confirmations: output.Confirmations,
			}
			witnessOutputs = append(witnessOutputs, utxo)
		}

	}

	return witnessOutputs, nil
}

// mapRpcclientError maps an error from the `btcwallet/chain` package to
// defined error in this package.
//
// NOTE: we are mapping the errors returned from `sendrawtransaction` RPC or
// the reject reason from `testmempoolaccept` RPC.
func mapRpcclientError(err error) error {
	// If we failed to publish the transaction, check whether we got an
	// error of known type.
	switch {
	// If the wallet reports a double spend, convert it to our internal
	// ErrDoubleSpend and return.
	case errors.Is(err, chain.ErrMempoolConflict),
		errors.Is(err, chain.ErrMissingInputs),
		errors.Is(err, chain.ErrTxAlreadyKnown),
		errors.Is(err, chain.ErrTxAlreadyConfirmed):

		return lnwallet.ErrDoubleSpend

	// If the wallet reports that fee requirements for accepting the tx
	// into mempool are not met, convert it to our internal ErrMempoolFee
	// and return.
	case errors.Is(err, chain.ErrMempoolMinFeeNotMet),
		errors.Is(err, chain.ErrMinRelayFeeNotMet):

		return fmt.Errorf("%w: %v", lnwallet.ErrMempoolFee, err.Error())
	}

	return err
}

// PublishTransaction performs cursory validation (dust checks, etc), then
// finally broadcasts the passed transaction to the Bitcoin network. If
// publishing the transaction fails, an error describing the reason is returned
// and mapped to the wallet's internal error types. If the transaction is
// already published to the network (either in the mempool or chain) no error
// will be returned.
func (b *BtcWallet) PublishTransaction(tx *wire.MsgTx, label string) error {
	// For neutrino backend there's no mempool, so we return early by
	// publishing the transaction.
	if b.chain.BackEnd() == "neutrino" {
		err := b.wallet.PublishTransaction(tx, label)

		return mapRpcclientError(err)
	}

	// For non-neutrino nodes, we will first check whether the transaction
	// can be accepted by the mempool.
	// Use a max feerate of 0 means the default value will be used when
	// testing mempool acceptance. The default max feerate is 0.10 BTC/kvb,
	// or 10,000 sat/vb.
	results, err := b.chain.TestMempoolAccept([]*wire.MsgTx{tx}, 0)
	if err != nil {
		// If the chain backend doesn't support the mempool acceptance
		// test RPC, we'll just attempt to publish the transaction.
		if errors.Is(err, rpcclient.ErrBackendVersion) {
			log.Warnf("TestMempoolAccept not supported by "+
				"backend, consider upgrading %s to a newer "+
				"version", b.chain.BackEnd())

			err := b.wallet.PublishTransaction(tx, label)

			return mapRpcclientError(err)
		}

		return err
	}

	// Sanity check that the expected single result is returned.
	if len(results) != 1 {
		return fmt.Errorf("expected 1 result from TestMempoolAccept, "+
			"instead got %v", len(results))
	}

	result := results[0]
	log.Debugf("TestMempoolAccept result: %s",
		lnutils.SpewLogClosure(result))

	// Once mempool check passed, we can publish the transaction.
	if result.Allowed {
		err = b.wallet.PublishTransaction(tx, label)

		return mapRpcclientError(err)
	}

	// If the check failed, there's no need to publish it. We'll handle the
	// error and return.
	log.Warnf("Transaction %v not accepted by mempool: %v",
		tx.TxHash(), result.RejectReason)

	// We need to use the string to create an error type and map it to a
	// btcwallet error.
	err = b.chain.MapRPCErr(errors.New(result.RejectReason))

	//nolint:ll
	// These two errors are ignored inside `PublishTransaction`:
	// https://github.com/btcsuite/btcwallet/blob/master/wallet/wallet.go#L3763
	// To keep our current behavior, we need to ignore the same errors
	// returned from TestMempoolAccept.
	//
	// TODO(yy): since `LightningWallet.PublishTransaction` always publish
	// the same tx twice, we'd always get ErrTxAlreadyInMempool. We should
	// instead create a new rebroadcaster that monitors the mempool, and
	// only rebroadcast when the tx is evicted. This way we don't need to
	// broadcast twice, and can instead return these errors here.
	switch {
	// NOTE: In addition to ignoring these errors, we need to call
	// `PublishTransaction` again because we need to mark the label in the
	// wallet. We can remove this exception once we have the above TODO
	// fixed.
	case errors.Is(err, chain.ErrTxAlreadyInMempool),
		errors.Is(err, chain.ErrTxAlreadyKnown),
		errors.Is(err, chain.ErrTxAlreadyConfirmed):

		err := b.wallet.PublishTransaction(tx, label)
		return mapRpcclientError(err)
	}

	return mapRpcclientError(err)
}

// LabelTransaction adds a label to a transaction. If the tx already
// has a label, this call will fail unless the overwrite parameter
// is set. Labels must not be empty, and they are limited to 500 chars.
//
// Note: it is part of the WalletController interface.
func (b *BtcWallet) LabelTransaction(hash chainhash.Hash, label string,
	overwrite bool) error {

	return b.wallet.LabelTransaction(hash, label, overwrite)
}

// extractBalanceDelta extracts the net balance delta from the PoV of the
// wallet given a TransactionSummary.
func extractBalanceDelta(
	txSummary base.TransactionSummary,
	tx *wire.MsgTx,
) (btcutil.Amount, error) {
	// For each input we debit the wallet's outflow for this transaction,
	// and for each output we credit the wallet's inflow for this
	// transaction.
	var balanceDelta btcutil.Amount
	for _, input := range txSummary.MyInputs {
		balanceDelta -= input.PreviousAmount
	}
	for _, output := range txSummary.MyOutputs {
		balanceDelta += btcutil.Amount(tx.TxOut[output.Index].Value)
	}

	return balanceDelta, nil
}

// getPreviousOutpoints is a helper function which gets the previous
// outpoints of a transaction.
func getPreviousOutpoints(wireTx *wire.MsgTx,
	myInputs []base.TransactionSummaryInput) []lnwallet.PreviousOutPoint {

	// isOurOutput is a map containing the output indices
	// controlled by the wallet.
	// Note: We make use of the information in `myInputs` provided
	// by the `wallet.TransactionSummary` structure that holds
	// information only if the input/previous_output is controlled by the wallet.
	isOurOutput := make(map[uint32]bool, len(myInputs))
	for _, myInput := range myInputs {
		isOurOutput[myInput.Index] = true
	}

	previousOutpoints := make([]lnwallet.PreviousOutPoint, len(wireTx.TxIn))
	for idx, txIn := range wireTx.TxIn {
		previousOutpoints[idx] = lnwallet.PreviousOutPoint{
			OutPoint:    txIn.PreviousOutPoint.String(),
			IsOurOutput: isOurOutput[uint32(idx)],
		}
	}

	return previousOutpoints
}

// GetTransactionDetails returns details of a transaction given its
// transaction hash.
func (b *BtcWallet) GetTransactionDetails(
	txHash *chainhash.Hash) (*lnwallet.TransactionDetail, error) {

	// Grab the best block the wallet knows of, we'll use this to calculate
	// # of confirmations shortly below.
	bestBlock := b.wallet.SyncedTo()
	currentHeight := bestBlock.Height
	tx, err := b.wallet.GetTransaction(*txHash)
	if err != nil {
		return nil, err
	}

	// For both confirmed and unconfirmed transactions, create a
	// TransactionDetail which re-packages the data returned by the base
	// wallet.
	if tx.Confirmations > 0 {
		txDetails, err := minedTransactionsToDetails(
			currentHeight,
			base.Block{
				Transactions: []base.TransactionSummary{
					tx.Summary,
				},
				Hash:      tx.BlockHash,
				Height:    tx.Height,
				Timestamp: tx.Summary.Timestamp},
			b.netParams,
		)
		if err != nil {
			return nil, err
		}

		return txDetails[0], nil
	}

	return unminedTransactionsToDetail(tx.Summary, b.netParams)
}

// minedTransactionsToDetails is a helper function which converts a summary
// information about mined transactions to a TransactionDetail.
func minedTransactionsToDetails(
	currentHeight int32,
	block base.Block,
	chainParams *chaincfg.Params,
) ([]*lnwallet.TransactionDetail, error) {

	details := make([]*lnwallet.TransactionDetail, 0, len(block.Transactions))
	for _, tx := range block.Transactions {
		wireTx := &wire.MsgTx{}
		txReader := bytes.NewReader(tx.Transaction)

		if err := wireTx.Deserialize(txReader); err != nil {
			return nil, err
		}

		// isOurAddress is a map containing the output indices
		// controlled by the wallet.
		// Note: We make use of the information in `MyOutputs` provided
		// by the `wallet.TransactionSummary` structure that holds
		// information only if the output is controlled by the wallet.
		isOurAddress := make(map[int]bool, len(tx.MyOutputs))
		for _, o := range tx.MyOutputs {
			isOurAddress[int(o.Index)] = true
		}

		var outputDetails []lnwallet.OutputDetail
		for i, txOut := range wireTx.TxOut {
			var addresses []btcutil.Address
			sc, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
				txOut.PkScript, chainParams,
			)
			if err == nil {
				// Add supported addresses.
				addresses = outAddresses
			}

			outputDetails = append(outputDetails, lnwallet.OutputDetail{
				OutputType:   sc,
				Addresses:    addresses,
				PkScript:     txOut.PkScript,
				OutputIndex:  i,
				Value:        btcutil.Amount(txOut.Value),
				IsOurAddress: isOurAddress[i],
			})
		}

		previousOutpoints := getPreviousOutpoints(wireTx, tx.MyInputs)

		txDetail := &lnwallet.TransactionDetail{
			Hash:              *tx.Hash,
			NumConfirmations:  currentHeight - block.Height + 1,
			BlockHash:         block.Hash,
			BlockHeight:       block.Height,
			Timestamp:         block.Timestamp,
			TotalFees:         int64(tx.Fee),
			OutputDetails:     outputDetails,
			RawTx:             tx.Transaction,
			Label:             tx.Label,
			PreviousOutpoints: previousOutpoints,
		}

		balanceDelta, err := extractBalanceDelta(tx, wireTx)
		if err != nil {
			return nil, err
		}
		txDetail.Value = balanceDelta

		details = append(details, txDetail)
	}

	return details, nil
}

// unminedTransactionsToDetail is a helper function which converts a summary
// for an unconfirmed transaction to a transaction detail.
func unminedTransactionsToDetail(
	summary base.TransactionSummary,
	chainParams *chaincfg.Params,
) (*lnwallet.TransactionDetail, error) {

	wireTx := &wire.MsgTx{}
	txReader := bytes.NewReader(summary.Transaction)

	if err := wireTx.Deserialize(txReader); err != nil {
		return nil, err
	}

	// isOurAddress is a map containing the output indices controlled by
	// the wallet.
	// Note: We make use of the information in `MyOutputs` provided
	// by the `wallet.TransactionSummary` structure that holds information
	// only if the output is controlled by the wallet.
	isOurAddress := make(map[int]bool, len(summary.MyOutputs))
	for _, o := range summary.MyOutputs {
		isOurAddress[int(o.Index)] = true
	}

	var outputDetails []lnwallet.OutputDetail
	for i, txOut := range wireTx.TxOut {
		var addresses []btcutil.Address
		sc, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, chainParams,
		)
		if err == nil {
			// Add supported addresses.
			addresses = outAddresses
		}

		outputDetails = append(outputDetails, lnwallet.OutputDetail{
			OutputType:   sc,
			Addresses:    addresses,
			PkScript:     txOut.PkScript,
			OutputIndex:  i,
			Value:        btcutil.Amount(txOut.Value),
			IsOurAddress: isOurAddress[i],
		})
	}

	previousOutpoints := getPreviousOutpoints(wireTx, summary.MyInputs)

	txDetail := &lnwallet.TransactionDetail{
		Hash:              *summary.Hash,
		TotalFees:         int64(summary.Fee),
		Timestamp:         summary.Timestamp,
		OutputDetails:     outputDetails,
		RawTx:             summary.Transaction,
		Label:             summary.Label,
		PreviousOutpoints: previousOutpoints,
	}

	balanceDelta, err := extractBalanceDelta(summary, wireTx)
	if err != nil {
		return nil, err
	}
	txDetail.Value = balanceDelta

	return txDetail, nil
}

// ListTransactionDetails returns a list of all transactions which are relevant
// to the wallet over [startHeight;endHeight]. If start height is greater than
// end height, the transactions will be retrieved in reverse order. To include
// unconfirmed transactions, endHeight should be set to the special value -1.
// This will return transactions from the tip of the chain until the start
// height (inclusive) and unconfirmed transactions. The account parameter serves
// as a filter to retrieve the transactions relevant to a specific account. When
// empty, transactions of all wallet accounts are returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListTransactionDetails(startHeight, endHeight int32,
	accountFilter string, indexOffset uint32,
	maxTransactions uint32) ([]*lnwallet.TransactionDetail, uint64, uint64,
	error) {

	// Grab the best block the wallet knows of, we'll use this to calculate
	// # of confirmations shortly below.
	bestBlock := b.wallet.SyncedTo()
	currentHeight := bestBlock.Height

	// We'll attempt to find all transactions from start to end height.
	start := base.NewBlockIdentifierFromHeight(startHeight)
	stop := base.NewBlockIdentifierFromHeight(endHeight)
	txns, err := b.wallet.GetTransactions(start, stop, accountFilter, nil)
	if err != nil {
		return nil, 0, 0, err
	}

	txDetails := make([]*lnwallet.TransactionDetail, 0,
		len(txns.MinedTransactions)+len(txns.UnminedTransactions))

	// For both confirmed and unconfirmed transactions, create a
	// TransactionDetail which re-packages the data returned by the base
	// wallet.
	for _, blockPackage := range txns.MinedTransactions {
		details, err := minedTransactionsToDetails(
			currentHeight, blockPackage, b.netParams,
		)
		if err != nil {
			return nil, 0, 0, err
		}

		txDetails = append(txDetails, details...)
	}
	for _, tx := range txns.UnminedTransactions {
		detail, err := unminedTransactionsToDetail(tx, b.netParams)
		if err != nil {
			return nil, 0, 0, err
		}

		txDetails = append(txDetails, detail)
	}

	// Return empty transaction list, if offset is more than all
	// transactions.
	if int(indexOffset) >= len(txDetails) {
		txDetails = []*lnwallet.TransactionDetail{}

		return txDetails, 0, 0, nil
	}

	end := indexOffset + maxTransactions

	// If maxTransactions is set to 0, then we'll return all transactions
	// starting from the offset.
	if maxTransactions == 0 {
		end = uint32(len(txDetails))
		txDetails = txDetails[indexOffset:end]

		return txDetails, uint64(indexOffset), uint64(end - 1), nil
	}

	if end > uint32(len(txDetails)) {
		end = uint32(len(txDetails))
	}

	txDetails = txDetails[indexOffset:end]

	return txDetails, uint64(indexOffset), uint64(end - 1), nil
}

// txSubscriptionClient encapsulates the transaction notification client from
// the base wallet. Notifications received from the client will be proxied over
// two distinct channels.
type txSubscriptionClient struct {
	txClient base.TransactionNotificationsClient

	confirmed   chan *lnwallet.TransactionDetail
	unconfirmed chan *lnwallet.TransactionDetail

	w base.Interface

	wg   sync.WaitGroup
	quit chan struct{}
}

// ConfirmedTransactions returns a channel which will be sent on as new
// relevant transactions are confirmed.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) ConfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.confirmed
}

// UnconfirmedTransactions returns a channel which will be sent on as
// new relevant transactions are seen within the network.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) UnconfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.unconfirmed
}

// Cancel finalizes the subscription, cleaning up any resources allocated.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) Cancel() {
	close(t.quit)
	t.wg.Wait()

	t.txClient.Done()
}

// notificationProxier proxies the notifications received by the underlying
// wallet's notification client to a higher-level TransactionSubscription
// client.
func (t *txSubscriptionClient) notificationProxier() {
	defer t.wg.Done()

out:
	for {
		select {
		case txNtfn := <-t.txClient.C:
			// TODO(roasbeef): handle detached blocks
			currentHeight := t.w.SyncedTo().Height

			// Launch a goroutine to re-package and send
			// notifications for any newly confirmed transactions.
			//nolint:ll
			go func(txNtfn *base.TransactionNotifications) {
				for _, block := range txNtfn.AttachedBlocks {
					details, err := minedTransactionsToDetails(
						currentHeight, block,
						t.w.ChainParams(),
					)
					if err != nil {
						continue
					}

					for _, d := range details {
						select {
						case t.confirmed <- d:
						case <-t.quit:
							return
						}
					}
				}
			}(txNtfn)

			// Launch a goroutine to re-package and send
			// notifications for any newly unconfirmed transactions.
			go func(txNtfn *base.TransactionNotifications) {
				for _, tx := range txNtfn.UnminedTransactions {
					detail, err := unminedTransactionsToDetail(
						tx, t.w.ChainParams(),
					)
					if err != nil {
						continue
					}

					select {
					case t.unconfirmed <- detail:
					case <-t.quit:
						return
					}
				}
			}(txNtfn)
		case <-t.quit:
			break out
		}
	}
}

// SubscribeTransactions returns a TransactionSubscription client which
// is capable of receiving async notifications as new transactions
// related to the wallet are seen within the network, or found in
// blocks.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	walletClient := b.wallet.NotificationServer().TransactionNotifications()

	txClient := &txSubscriptionClient{
		txClient:    walletClient,
		confirmed:   make(chan *lnwallet.TransactionDetail),
		unconfirmed: make(chan *lnwallet.TransactionDetail),
		w:           b.wallet,
		quit:        make(chan struct{}),
	}
	txClient.wg.Add(1)
	go txClient.notificationProxier()

	return txClient, nil
}

// IsSynced returns a boolean indicating if from the PoV of the wallet, it has
// fully synced to the current best block in the main chain.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) IsSynced() (bool, int64, error) {
	// Grab the best chain state the wallet is currently aware of.
	syncState := b.wallet.SyncedTo()

	// We'll also extract the current best wallet timestamp so the caller
	// can get an idea of where we are in the sync timeline.
	bestTimestamp := syncState.Timestamp.Unix()

	// Next, query the chain backend to grab the info about the tip of the
	// main chain.
	bestHash, bestHeight, err := b.cfg.ChainSource.GetBestBlock()
	if err != nil {
		return false, 0, err
	}

	// Make sure the backing chain has been considered synced first.
	if !b.wallet.ChainSynced() {
		bestHeader, err := b.cfg.ChainSource.GetBlockHeader(bestHash)
		if err != nil {
			return false, 0, err
		}
		bestTimestamp = bestHeader.Timestamp.Unix()
		return false, bestTimestamp, nil
	}

	// If the wallet hasn't yet fully synced to the node's best chain tip,
	// then we're not yet fully synced.
	if syncState.Height < bestHeight {
		return false, bestTimestamp, nil
	}

	// If the wallet is on par with the current best chain tip, then we
	// still may not yet be synced as the chain backend may still be
	// catching up to the main chain. So we'll grab the block header in
	// order to make a guess based on the current time stamp.
	blockHeader, err := b.cfg.ChainSource.GetBlockHeader(bestHash)
	if err != nil {
		return false, 0, err
	}

	// If the timestamp on the best header is more than 2 hours in the
	// past, then we're not yet synced.
	minus24Hours := time.Now().Add(-2 * time.Hour)
	if blockHeader.Timestamp.Before(minus24Hours) {
		return false, bestTimestamp, nil
	}

	return true, bestTimestamp, nil
}

// GetRecoveryInfo returns a boolean indicating whether the wallet is started
// in recovery mode. It also returns a float64, ranging from 0 to 1,
// representing the recovery progress made so far.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) GetRecoveryInfo() (bool, float64, error) {
	isRecoveryMode := true
	progress := float64(0)

	// A zero value in RecoveryWindow indicates there is no trigger of
	// recovery mode.
	if b.cfg.RecoveryWindow == 0 {
		isRecoveryMode = false
		return isRecoveryMode, progress, nil
	}

	// Query the wallet's birthday block from db.
	birthdayBlock, err := b.wallet.BirthdayBlock()
	if err != nil {
		// The wallet won't start until the backend is synced, thus the birthday
		// block won't be set and this particular error will be returned. We'll
		// catch this error and return a progress of 0 instead.
		if waddrmgr.IsError(err, waddrmgr.ErrBirthdayBlockNotSet) {
			return isRecoveryMode, progress, nil
		}

		return isRecoveryMode, progress, err
	}

	// Grab the best chain state the wallet is currently aware of.
	syncState := b.wallet.SyncedTo()

	// Next, query the chain backend to grab the info about the tip of the
	// main chain.
	//
	// NOTE: The actual recovery process is handled by the btcsuite/btcwallet.
	// The process purposefully doesn't update the best height. It might create
	// a small difference between the height queried here and the height used
	// in the recovery process, ie, the bestHeight used here might be greater,
	// showing the recovery being unfinished while it's actually done. However,
	// during a wallet rescan after the recovery, the wallet's synced height
	// will catch up and this won't be an issue.
	_, bestHeight, err := b.cfg.ChainSource.GetBestBlock()
	if err != nil {
		return isRecoveryMode, progress, err
	}

	// The birthday block height might be greater than the current synced height
	// in a newly restored wallet, and might be greater than the chain tip if a
	// rollback happens. In that case, we will return zero progress here.
	if syncState.Height < birthdayBlock.Height ||
		bestHeight < birthdayBlock.Height {

		return isRecoveryMode, progress, nil
	}

	// progress is the ratio of the [number of blocks processed] over the [total
	// number of blocks] needed in a recovery mode, ranging from 0 to 1, in
	// which,
	// - total number of blocks is the current chain's best height minus the
	//   wallet's birthday height plus 1.
	// - number of blocks processed is the wallet's synced height minus its
	//   birthday height plus 1.
	// - If the wallet is born very recently, the bestHeight can be equal to
	//   the birthdayBlock.Height, and it will recovery instantly.
	progress = float64(syncState.Height-birthdayBlock.Height+1) /
		float64(bestHeight-birthdayBlock.Height+1)

	return isRecoveryMode, progress, nil
}

// FetchTx attempts to fetch a transaction in the wallet's database identified
// by the passed transaction hash. If the transaction can't be found, then a
// nil pointer is returned.
func (b *BtcWallet) FetchTx(txHash chainhash.Hash) (*wire.MsgTx, error) {
	tx, err := b.wallet.GetTransaction(txHash)
	if err != nil {
		return nil, err
	}

	return tx.Summary.Tx, nil
}

// RemoveDescendants attempts to remove any transaction from the wallet's tx
// store (that may be unconfirmed) that spends outputs created by the passed
// transaction. This remove propagates recursively down the chain of descendent
// transactions.
func (b *BtcWallet) RemoveDescendants(tx *wire.MsgTx) error {
	return b.wallet.RemoveDescendants(tx)
}

// CheckMempoolAcceptance is a wrapper around `TestMempoolAccept` which checks
// the mempool acceptance of a transaction.
func (b *BtcWallet) CheckMempoolAcceptance(tx *wire.MsgTx) error {
	// Use a max feerate of 0 means the default value will be used when
	// testing mempool acceptance. The default max feerate is 0.10 BTC/kvb,
	// or 10,000 sat/vb.
	results, err := b.chain.TestMempoolAccept([]*wire.MsgTx{tx}, 0)
	if err != nil {
		return err
	}

	// Sanity check that the expected single result is returned.
	if len(results) != 1 {
		return fmt.Errorf("expected 1 result from TestMempoolAccept, "+
			"instead got %v", len(results))
	}

	result := results[0]
	log.Debugf("TestMempoolAccept result: %s",
		lnutils.SpewLogClosure(result))

	// Mempool check failed, we now map the reject reason to a proper RPC
	// error and return it.
	if !result.Allowed {
		err := b.chain.MapRPCErr(errors.New(result.RejectReason))

		return fmt.Errorf("mempool rejection: %w", err)
	}

	return nil
}
