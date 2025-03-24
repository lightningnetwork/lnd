package lnwallet

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanvalidate"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultAccountName is the name for the default account used to manage
	// on-chain funds within the wallet.
	DefaultAccountName = "default"
)

// AddressType is an enum-like type which denotes the possible address types
// WalletController supports.
type AddressType uint8

// AccountAddressMap maps the account properties to an array of
// address properties.
type AccountAddressMap map[*waddrmgr.AccountProperties][]AddressProperty

const (
	// UnknownAddressType represents an output with an unknown or non-standard
	// script.
	UnknownAddressType AddressType = iota

	// WitnessPubKey represents a p2wkh address.
	WitnessPubKey

	// NestedWitnessPubKey represents a p2sh output which is itself a
	// nested p2wkh output.
	NestedWitnessPubKey

	// TaprootPubkey represents a p2tr key path spending address.
	TaprootPubkey
)

var (
	// DefaultPublicPassphrase is the default public passphrase used for the
	// wallet.
	DefaultPublicPassphrase = []byte("public")

	// DefaultPrivatePassphrase is the default private passphrase used for
	// the wallet.
	DefaultPrivatePassphrase = []byte("hello")

	// ErrDoubleSpend is returned from PublishTransaction in case the
	// tx being published is spending an output spent by a conflicting
	// transaction.
	ErrDoubleSpend = errors.New("transaction rejected: output already spent")

	// ErrNotMine is an error denoting that a WalletController instance is
	// unable to spend a specified output.
	ErrNotMine = errors.New("the passed output doesn't belong to the wallet")

	// ErrMempoolFee is returned from PublishTransaction in case the tx
	// being published is not accepted into mempool because the fee
	// requirements of the mempool backend are not met.
	ErrMempoolFee = errors.New("transaction rejected by the mempool " +
		"because of low fees")
)

// ErrNoOutputs is returned if we try to create a transaction with no outputs
// or send coins to a set of outputs that is empty.
var ErrNoOutputs = errors.New("no outputs")

// ErrInvalidMinconf is returned if we try to create a transaction with
// invalid minConfs value.
var ErrInvalidMinconf = errors.New("minimum number of confirmations must " +
	"be a non-negative number")

// AddressProperty contains wallet related information of an address.
type AddressProperty struct {
	// Address is the address of an account.
	Address string

	// Internal denotes if the address is a change address.
	Internal bool

	// Balance returns the total balance of an address.
	Balance btcutil.Amount

	// DerivationPath is the derivation path of the address.
	DerivationPath string

	// PublicKey is the public key of the address.
	PublicKey *btcec.PublicKey
}

// AccountIdentifier contains information to uniquely identify an account.
type AccountIdentifier struct {
	// Name is the name of the account.
	Name string

	// AddressType is the type of addresses supported by the account.
	AddressType AddressType

	// DerivationPath is the derivation path corresponding to the account
	// public key.
	DerivationPath string
}

// Utxo is an unspent output denoted by its outpoint, and output value of the
// original output.
type Utxo struct {
	AddressType   AddressType
	Value         btcutil.Amount
	Confirmations int64
	PkScript      []byte
	wire.OutPoint
	PrevTx *wire.MsgTx
}

// OutputDetail contains additional information on a destination address.
type OutputDetail struct {
	OutputType   txscript.ScriptClass
	Addresses    []btcutil.Address
	PkScript     []byte
	OutputIndex  int
	Value        btcutil.Amount
	IsOurAddress bool
}

// PreviousOutPoint contains information about the previous outpoint.
type PreviousOutPoint struct {
	// OutPoint is the transaction out point in the format txid:n.
	OutPoint string

	// IsOurOutput denotes if the previous output is controlled by the
	// internal wallet. The flag will only detect p2wkh, np2wkh and p2tr
	// inputs as its own.
	IsOurOutput bool
}

// TransactionDetail describes a transaction with either inputs which belong to
// the wallet, or has outputs that pay to the wallet.
type TransactionDetail struct {
	// Hash is the transaction hash of the transaction.
	Hash chainhash.Hash

	// Value is the net value of this transaction (in satoshis) from the
	// PoV of the wallet. If this transaction purely spends from the
	// wallet's funds, then this value will be negative. Similarly, if this
	// transaction credits the wallet, then this value will be positive.
	Value btcutil.Amount

	// NumConfirmations is the number of confirmations this transaction
	// has. If the transaction is unconfirmed, then this value will be
	// zero.
	NumConfirmations int32

	// BlockHeight is the hash of the block which includes this
	// transaction. Unconfirmed transactions will have a nil value for this
	// field.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block including this transaction.
	// Unconfirmed transaction will show a height of zero.
	BlockHeight int32

	// Timestamp is the unix timestamp of the block including this
	// transaction. If the transaction is unconfirmed, then this will be a
	// timestamp of txn creation.
	Timestamp int64

	// TotalFees is the total fee in satoshis paid by this transaction.
	TotalFees int64

	// OutputDetails contains output data for each destination address, such
	// as the output script and amount.
	OutputDetails []OutputDetail

	// RawTx returns the raw serialized transaction.
	RawTx []byte

	// Label is an optional transaction label.
	Label string

	// PreviousOutpoints are the inputs for a transaction.
	PreviousOutpoints []PreviousOutPoint
}

// TransactionSubscription is an interface which describes an object capable of
// receiving notifications of new transaction related to the underlying wallet.
// TODO(roasbeef): add balance updates?
type TransactionSubscription interface {
	// ConfirmedTransactions returns a channel which will be sent on as new
	// relevant transactions are confirmed.
	ConfirmedTransactions() chan *TransactionDetail

	// UnconfirmedTransactions returns a channel which will be sent on as
	// new relevant transactions are seen within the network.
	UnconfirmedTransactions() chan *TransactionDetail

	// Cancel finalizes the subscription, cleaning up any resources
	// allocated.
	Cancel()
}

// WalletController defines an abstract interface for controlling a local Pure
// Go wallet, a local or remote wallet via an RPC mechanism, or possibly even
// a daemon assisted hardware wallet. This interface serves the purpose of
// allowing LightningWallet to be seamlessly compatible with several wallets
// such as: uspv, btcwallet, Bitcoin Core, Electrum, etc. This interface then
// serves as a "base wallet", with Lightning Network awareness taking place at
// a "higher" level of abstraction. Essentially, an overlay wallet.
// Implementors of this interface must closely adhere to the documented
// behavior of all interface methods in order to ensure identical behavior
// across all concrete implementations.
type WalletController interface {
	// FetchOutpointInfo queries for the WalletController's knowledge of
	// the passed outpoint. If the base wallet determines this output is
	// under its control, then the original txout should be returned.
	// Otherwise, a non-nil error value of ErrNotMine should be returned
	// instead.
	FetchOutpointInfo(prevOut *wire.OutPoint) (*Utxo, error)

	// FetchDerivationInfo queries for the wallet's knowledge of the passed
	// pkScript and constructs the derivation info and returns it.
	FetchDerivationInfo(pkScript []byte) (*psbt.Bip32Derivation, error)

	// ScriptForOutput returns the address, witness program and redeem
	// script for a given UTXO. An error is returned if the UTXO does not
	// belong to our wallet or it is not a managed pubKey address.
	ScriptForOutput(output *wire.TxOut) (waddrmgr.ManagedPubKeyAddress,
		[]byte, []byte, error)

	// ConfirmedBalance returns the sum of all the wallet's unspent outputs
	// that have at least confs confirmations. If confs is set to zero,
	// then all unspent outputs, including those currently in the mempool
	// will be included in the final sum. The account parameter serves as a
	// filter to retrieve the balance for a specific account. When empty,
	// the confirmed balance of all wallet accounts is returned.
	//
	// NOTE: Only witness outputs should be included in the computation of
	// the total spendable balance of the wallet. We require this as only
	// witness inputs can be used for funding channels.
	ConfirmedBalance(confs int32, accountFilter string) (btcutil.Amount,
		error)

	// NewAddress returns the next external or internal address for the
	// wallet dictated by the value of the `change` parameter. If change is
	// true, then an internal address should be used, otherwise an external
	// address should be returned. The type of address returned is dictated
	// by the wallet's capabilities, and may be of type: p2sh, p2wkh,
	// p2wsh, etc. The account parameter must be non-empty as it determines
	// which account the address should be generated from.
	NewAddress(addrType AddressType, change bool,
		account string) (btcutil.Address, error)

	// LastUnusedAddress returns the last *unused* address known by the
	// wallet. An address is unused if it hasn't received any payments.
	// This can be useful in UIs in order to continually show the
	// "freshest" address without having to worry about "address inflation"
	// caused by continual refreshing. Similar to NewAddress it can derive
	// a specified address type. By default, this is a non-change address.
	// The account parameter must be non-empty as it determines which
	// account the address should be generated from.
	LastUnusedAddress(addrType AddressType,
		account string) (btcutil.Address, error)

	// IsOurAddress checks if the passed address belongs to this wallet
	IsOurAddress(a btcutil.Address) bool

	// AddressInfo returns the information about an address, if it's known
	// to this wallet.
	AddressInfo(a btcutil.Address) (waddrmgr.ManagedAddress, error)

	// ListAccounts retrieves all accounts belonging to the wallet by
	// default. A name and key scope filter can be provided to filter
	// through all of the wallet accounts and return only those matching.
	ListAccounts(string, *waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties,
		error)

	// RequiredReserve returns the minimum amount of satoshis that should be
	// kept in the wallet in order to fee bump anchor channels if necessary.
	// The value scales with the number of public anchor channels but is
	// capped at a maximum.
	RequiredReserve(uint32) btcutil.Amount

	// ListAddresses retrieves all the addresses along with their balance. An
	// account name filter can be provided to filter through all of the
	// wallet accounts and return the addresses of only those matching.
	ListAddresses(string, bool) (AccountAddressMap, error)

	// ImportAccount imports an account backed by an account extended public
	// key. The master key fingerprint denotes the fingerprint of the root
	// key corresponding to the account public key (also known as the key
	// with derivation path m/). This may be required by some hardware
	// wallets for proper identification and signing.
	//
	// The address type can usually be inferred from the key's version, but
	// may be required for certain keys to map them into the proper scope.
	//
	// For BIP-0044 keys, an address type must be specified as we intend to
	// not support importing BIP-0044 keys into the wallet using the legacy
	// pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will
	// force the standard BIP-0049 derivation scheme, while a witness
	// address type will force the standard BIP-0084 derivation scheme.
	//
	// For BIP-0049 keys, an address type must also be specified to make a
	// distinction between the standard BIP-0049 address schema (nested
	// witness pubkeys everywhere) and our own BIP-0049Plus address schema
	// (nested pubkeys externally, witness pubkeys internally).
	ImportAccount(name string, accountPubKey *hdkeychain.ExtendedKey,
		masterKeyFingerprint uint32, addrType *waddrmgr.AddressType,
		dryRun bool) (*waddrmgr.AccountProperties, []btcutil.Address,
		[]btcutil.Address, error)

	// ImportPublicKey imports a single derived public key into the wallet.
	// The address type can usually be inferred from the key's version, but
	// in the case of legacy versions (xpub, tpub), an address type must be
	// specified as we intend to not support importing BIP-44 keys into the
	// wallet using the legacy pay-to-pubkey-hash (P2PKH) scheme.
	ImportPublicKey(pubKey *btcec.PublicKey,
		addrType waddrmgr.AddressType) error

	// ImportTaprootScript imports a user-provided taproot script into the
	// wallet. The imported script will act as a pay-to-taproot address.
	//
	// NOTE: Taproot keys imported through this RPC currently _cannot_ be
	// used for funding PSBTs. Only tracking the balance and UTXOs is
	// currently supported.
	ImportTaprootScript(scope waddrmgr.KeyScope,
		tapscript *waddrmgr.Tapscript) (waddrmgr.ManagedAddress, error)

	// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying
	// out to the specified outputs. In the case the wallet has insufficient
	// funds, or the outputs are non-standard, an error should be returned.
	// This method also takes the target fee expressed in sat/kw that should
	// be used when crafting the transaction.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	SendOutputs(inputs fn.Set[wire.OutPoint], outputs []*wire.TxOut,
		feeRate chainfee.SatPerKWeight, minConfs int32, label string,
		strategy base.CoinSelectionStrategy) (*wire.MsgTx, error)

	// CreateSimpleTx creates a Bitcoin transaction paying to the specified
	// outputs. The transaction is not broadcasted to the network. In the
	// case the wallet has insufficient funds, or the outputs are
	// non-standard, an error should be returned. This method also takes
	// the target fee expressed in sat/kw that should be used when crafting
	// the transaction.
	//
	// NOTE: The dryRun argument can be set true to create a tx that
	// doesn't alter the database. A tx created with this set to true
	// SHOULD NOT be broadcasted.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	CreateSimpleTx(inputs fn.Set[wire.OutPoint], outputs []*wire.TxOut,
		feeRate chainfee.SatPerKWeight, minConfs int32,
		strategy base.CoinSelectionStrategy, dryRun bool) (
		*txauthor.AuthoredTx, error)

	// GetTransactionDetails returns a detailed description of a transaction
	// given its transaction hash.
	GetTransactionDetails(txHash *chainhash.Hash) (
		*TransactionDetail, error)

	// ListUnspentWitness returns all unspent outputs which are version 0
	// witness programs. The 'minConfs' and 'maxConfs' parameters
	// indicate the minimum and maximum number of confirmations an output
	// needs in order to be returned by this method. Passing -1 as
	// 'minConfs' indicates that even unconfirmed outputs should be
	// returned. Using MaxInt32 as 'maxConfs' implies returning all
	// outputs with at least 'minConfs'. The account parameter serves as
	// a filter to retrieve the unspent outputs for a specific account.
	// When empty, the unspent outputs of all wallet accounts are returned.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	ListUnspentWitness(minConfs, maxConfs int32,
		accountFilter string) ([]*Utxo, error)

	// ListTransactionDetails returns a list of all transactions which are
	// relevant to the wallet over [startHeight;endHeight]. If start height
	// is greater than end height, the transactions will be retrieved in
	// reverse order. To include unconfirmed transactions, endHeight should
	// be set to the special value -1. This will return transactions from
	// the tip of the chain until the start height (inclusive) and
	// unconfirmed transactions. The account parameter serves as a filter to
	// retrieve the transactions relevant to a specific account. When
	// empty, transactions of all wallet accounts are returned.
	ListTransactionDetails(startHeight, endHeight int32,
		accountFilter string, indexOffset uint32,
		maxTransactions uint32) ([]*TransactionDetail, uint64, uint64,
		error)

	// LeaseOutput locks an output to the given ID, preventing it from being
	// available for any future coin selection attempts. The absolute time
	// of the lock's expiration is returned. The expiration of the lock can
	// be extended by successive invocations of this call. Outputs can be
	// unlocked before their expiration through `ReleaseOutput`.
	//
	// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If
	// the output has already been locked to a different ID, then
	// wtxmgr.ErrOutputAlreadyLocked is returned.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
		duration time.Duration) (time.Time, error)

	// ReleaseOutput unlocks an output, allowing it to be available for coin
	// selection if it remains unspent. The ID should match the one used to
	// originally lock the output.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error

	// ListLeasedOutputs returns a list of all currently locked outputs.
	ListLeasedOutputs() ([]*base.ListLeasedOutputResult, error)

	// PublishTransaction performs cursory validation (dust checks, etc),
	// then finally broadcasts the passed transaction to the Bitcoin network.
	// If the transaction is rejected because it is conflicting with an
	// already known transaction, ErrDoubleSpend is returned. If the
	// transaction is already known (published already), no error will be
	// returned. Other error returned depends on the currently active chain
	// backend. It takes an optional label which will save a label with the
	// published transaction.
	PublishTransaction(tx *wire.MsgTx, label string) error

	// LabelTransaction adds a label to a transaction. If the tx already
	// has a label, this call will fail unless the overwrite parameter
	// is set. Labels must not be empty, and they are limited to 500 chars.
	LabelTransaction(hash chainhash.Hash, label string, overwrite bool) error

	// FetchTx attempts to fetch a transaction in the wallet's database
	// identified by the passed transaction hash. If the transaction can't
	// be found, then a nil pointer is returned.
	FetchTx(chainhash.Hash) (*wire.MsgTx, error)

	// RemoveDescendants attempts to remove any transaction from the
	// wallet's tx store (that may be unconfirmed) that spends outputs
	// created by the passed transaction. This remove propagates
	// recursively down the chain of descendent transactions.
	RemoveDescendants(*wire.MsgTx) error

	// FundPsbt creates a fully populated PSBT packet that contains enough
	// inputs to fund the outputs specified in the passed in packet with the
	// specified fee rate. If there is change left, a change output from the
	// internal wallet is added and the index of the change output is
	// returned. Otherwise no additional output is created and the index -1
	// is returned. If no custom change scope is specified, the BIP0084 will
	// be used for default accounts and single imported public keys. For
	// custom account, no key scope should be provided as the coin selection
	// key scope will always be used to generate the change address.
	//
	// NOTE: If the packet doesn't contain any inputs, coin selection is
	// performed automatically. The account parameter must be non-empty as
	// it determines which set of coins are eligible for coin selection. If
	// the packet does contain any inputs, it is assumed that full coin
	// selection happened externally and no additional inputs are added. If
	// the specified inputs aren't enough to fund the outputs with the given
	// fee rate, an error is returned. No lock lease is acquired for any of
	// the selected/validated inputs. It is in the caller's responsibility
	// to lock the inputs before handing them out.
	FundPsbt(packet *psbt.Packet, minConfs int32,
		feeRate chainfee.SatPerKWeight, account string,
		changeScope *waddrmgr.KeyScope,
		strategy base.CoinSelectionStrategy,
		allowUtxo func(wtxmgr.Credit) bool) (int32, error)

	// SignPsbt expects a partial transaction with all inputs and outputs
	// fully declared and tries to sign all unsigned inputs that have all
	// required fields (UTXO information, BIP32 derivation information,
	// witness or sig scripts) set.
	// If no error is returned, the PSBT is ready to be given to the next
	// signer or to be finalized if lnd was the last signer.
	//
	// NOTE: This method only signs inputs (and only those it can sign), it
	// does not perform any other tasks (such as coin selection, UTXO
	// locking or input/output/fee value validation, PSBT finalization). Any
	// input that is incomplete will be skipped.
	SignPsbt(packet *psbt.Packet) ([]uint32, error)

	// FinalizePsbt expects a partial transaction with all inputs and
	// outputs fully declared and tries to sign all inputs that belong to
	// the specified account. Lnd must be the last signer of the
	// transaction. That means, if there are any unsigned non-witness inputs
	// or inputs without UTXO information attached or inputs without witness
	// data that do not belong to lnd's wallet, this method will fail. If no
	// error is returned, the PSBT is ready to be extracted and the final TX
	// within to be broadcast.
	//
	// NOTE: This method does NOT publish the transaction after it's been
	// finalized successfully.
	FinalizePsbt(packet *psbt.Packet, account string) error

	// DecorateInputs fetches the UTXO information of all inputs it can
	// identify and adds the required information to the package's inputs.
	// The failOnUnknown boolean controls whether the method should return
	// an error if it cannot identify an input or if it should just skip it.
	DecorateInputs(packet *psbt.Packet, failOnUnknown bool) error

	// SubscribeTransactions returns a TransactionSubscription client which
	// is capable of receiving async notifications as new transactions
	// related to the wallet are seen within the network, or found in
	// blocks.
	//
	// NOTE: a non-nil error should be returned if notifications aren't
	// supported.
	//
	// TODO(roasbeef): make distinct interface?
	SubscribeTransactions() (TransactionSubscription, error)

	// IsSynced returns a boolean indicating if from the PoV of the wallet,
	// it has fully synced to the current best block in the main chain.
	// It also returns an int64 indicating the timestamp of the best block
	// known to the wallet, expressed in Unix epoch time
	IsSynced() (bool, int64, error)

	// GetRecoveryInfo returns a boolean indicating whether the wallet is
	// started in recovery mode. It also returns a float64 indicating the
	// recovery progress made so far.
	GetRecoveryInfo() (bool, float64, error)

	// Start initializes the wallet, making any necessary connections,
	// starting up required goroutines etc.
	Start() error

	// Stop signals the wallet for shutdown. Shutdown may entail closing
	// any active sockets, database handles, stopping goroutines, etc.
	Stop() error

	// BackEnd returns a name for the wallet's backing chain service,
	// which could be e.g. btcd, bitcoind, neutrino, or another consensus
	// service.
	BackEnd() string

	// CheckMempoolAcceptance checks whether a transaction follows mempool
	// policies and returns an error if it cannot be accepted into the
	// mempool.
	CheckMempoolAcceptance(tx *wire.MsgTx) error
}

// BlockChainIO is a dedicated source which will be used to obtain queries
// related to the current state of the blockchain. The data returned by each of
// the defined methods within this interface should always return the most up
// to date data possible.
//
// TODO(roasbeef): move to diff package perhaps?
// TODO(roasbeef): move publish txn here?
type BlockChainIO interface {
	// GetBestBlock returns the current height and block hash of the valid
	// most-work chain the implementation is aware of.
	GetBestBlock() (*chainhash.Hash, int32, error)

	// GetUtxo attempts to return the passed outpoint if it's still a
	// member of the utxo set. The passed height hint should be the "birth
	// height" of the passed outpoint. The script passed should be the
	// script that the outpoint creates. In the case that the output is in
	// the UTXO set, then the output corresponding to that output is
	// returned.  Otherwise, a non-nil error will be returned.
	// As for some backends this call can initiate a rescan, the passed
	// cancel channel can be closed to abort the call.
	GetUtxo(op *wire.OutPoint, pkScript []byte, heightHint uint32,
		cancel <-chan struct{}) (*wire.TxOut, error)

	// GetBlockHash returns the hash of the block in the best blockchain
	// at the given height.
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)

	// GetBlock returns the block in the main chain identified by the given
	// hash.
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)

	// GetBlockHeader returns the block header for the given block hash.
	GetBlockHeader(blockHash *chainhash.Hash) (*wire.BlockHeader, error)
}

// MessageSigner represents an abstract object capable of signing arbitrary
// messages. The capabilities of this interface are used to sign announcements
// to the network, or just arbitrary messages that leverage the wallet's keys
// to attest to some message.
type MessageSigner interface {
	// SignMessage attempts to sign a target message with the private key
	// described in the key locator. If the target private key is unable to
	// be found, then an error will be returned. The actual digest signed is
	// the single or double SHA-256 of the passed message.
	SignMessage(keyLoc keychain.KeyLocator, msg []byte,
		doubleHash bool) (*ecdsa.Signature, error)
}

// AddrWithKey wraps a normal addr, but also includes the internal key for the
// delivery addr if known.
type AddrWithKey struct {
	lnwire.DeliveryAddress

	InternalKey fn.Option[keychain.KeyDescriptor]

	// TODO(roasbeef): consolidate w/ instance in chan closer
}

// InternalKeyForAddr returns the internal key associated with a taproot
// address.
func InternalKeyForAddr(wallet WalletController, netParams *chaincfg.Params,
	deliveryScript []byte) (fn.Option[keychain.KeyDescriptor], error) {

	none := fn.None[keychain.KeyDescriptor]()

	pkScript, err := txscript.ParsePkScript(deliveryScript)
	if err != nil {
		return none, err
	}
	addr, err := pkScript.Address(netParams)
	if err != nil {
		return none, err
	}

	// If it's not a taproot address, we don't require to know the internal
	// key in the first place. So we don't return an error here, but also no
	// internal key.
	_, isTaproot := addr.(*btcutil.AddressTaproot)
	if !isTaproot {
		return none, nil
	}

	walletAddr, err := wallet.AddressInfo(addr)
	if err != nil {
		// If the error is that the address can't be found, it is not
		// an error. This happens when any channel which is not a custom
		// taproot channel is cooperatively closed to an external P2TR
		// address. In this case there is no internal key associated
		// with the address. Callers can use the .Option() method to get
		// an option value.
		var managerErr waddrmgr.ManagerError
		if errors.As(err, &managerErr) &&
			managerErr.ErrorCode == waddrmgr.ErrAddressNotFound {

			return none, nil
		}

		return none, err
	}

	// No wallet addr. No error, but we'll return an nil error value here,
	// as callers can use the .Option() method to get an option value.
	if walletAddr == nil {
		return none, nil
	}

	// Imported addresses do not provide private keys, so they do not
	// implement waddrmgr.ManagedPubKeyAddress. See RPC ImportTapscript.
	if walletAddr.Imported() {
		return none, nil
	}

	pubKeyAddr, ok := walletAddr.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		return none, fmt.Errorf("expected pubkey addr, got %T",
			walletAddr)
	}

	_, derivationPath, _ := pubKeyAddr.DerivationInfo()

	return fn.Some[keychain.KeyDescriptor](keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(derivationPath.Account),
			Index:  derivationPath.Index,
		},
		PubKey: pubKeyAddr.PubKey(),
	}), nil
}

// WalletDriver represents a "driver" for a particular concrete
// WalletController implementation. A driver is identified by a globally unique
// string identifier along with a 'New()' method which is responsible for
// initializing a particular WalletController concrete implementation.
type WalletDriver struct {
	// WalletType is a string which uniquely identifies the
	// WalletController that this driver, drives.
	WalletType string

	// New creates a new instance of a concrete WalletController
	// implementation given a variadic set up arguments. The function takes
	// a variadic number of interface parameters in order to provide
	// initialization flexibility, thereby accommodating several potential
	// WalletController implementations.
	New func(args ...interface{}) (WalletController, error)

	// BackEnds returns a list of available chain service drivers for the
	// wallet driver. This could be e.g. bitcoind, btcd, neutrino, etc.
	BackEnds func() []string
}

var (
	wallets     = make(map[string]*WalletDriver)
	registerMtx sync.Mutex
)

// RegisteredWallets returns a slice of all currently registered notifiers.
//
// NOTE: This function is safe for concurrent access.
func RegisteredWallets() []*WalletDriver {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	registeredWallets := make([]*WalletDriver, 0, len(wallets))
	for _, wallet := range wallets {
		registeredWallets = append(registeredWallets, wallet)
	}

	return registeredWallets
}

// RegisterWallet registers a WalletDriver which is capable of driving a
// concrete WalletController interface. In the case that this driver has
// already been registered, an error is returned.
//
// NOTE: This function is safe for concurrent access.
func RegisterWallet(driver *WalletDriver) error {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	if _, ok := wallets[driver.WalletType]; ok {
		return fmt.Errorf("wallet already registered")
	}

	wallets[driver.WalletType] = driver

	return nil
}

// SupportedWallets returns a slice of strings that represents the wallet
// drivers that have been registered and are therefore supported.
//
// NOTE: This function is safe for concurrent access.
func SupportedWallets() []string {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	supportedWallets := make([]string, 0, len(wallets))
	for walletName := range wallets {
		supportedWallets = append(supportedWallets, walletName)
	}

	return supportedWallets
}

// FetchFundingTxWrapper is a wrapper around FetchFundingTx, except that it will
// exit when the supplied quit channel is closed.
func FetchFundingTxWrapper(chain BlockChainIO, chanID lnwire.ShortChannelID,
	quit chan struct{}) (*wire.MsgTx, error) {

	txChan := make(chan *wire.MsgTx, 1)
	errChan := make(chan error, 1)

	go func() {
		tx, err := FetchFundingTx(chain, chanID)
		if err != nil {
			errChan <- err
			return
		}

		txChan <- tx
	}()

	select {
	case tx := <-txChan:
		return tx, nil

	case err := <-errChan:
		return nil, err

	case <-quit:
		return nil, fmt.Errorf("quit channel passed to " +
			"lnwallet.FetchFundingTxWrapper has been closed")
	}
}

// FetchFundingTx uses the given BlockChainIO to fetch and return the funding
// transaction identified by the passed short channel ID.
//
// TODO(roasbeef): replace with call to GetBlockTransaction? (would allow to
// later use getblocktxn).
func FetchFundingTx(chain BlockChainIO,
	chanID lnwire.ShortChannelID) (*wire.MsgTx, error) {

	// First fetch the block hash by the block number encoded, then use
	// that hash to fetch the block itself.
	blockNum := int64(chanID.BlockHeight)
	blockHash, err := chain.GetBlockHash(blockNum)
	if err != nil {
		return nil, err
	}

	fundingBlock, err := chain.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// As a sanity check, ensure that the advertised transaction index is
	// within the bounds of the total number of transactions within a
	// block.
	numTxns := uint32(len(fundingBlock.Transactions))
	if chanID.TxIndex > numTxns-1 {
		return nil, fmt.Errorf("tx_index=#%v "+
			"is out of range (max_index=%v), network_chan_id=%v",
			chanID.TxIndex, numTxns-1, chanID)
	}

	return fundingBlock.Transactions[chanID.TxIndex].Copy(), nil
}

// FetchPKScriptWithQuit fetches the output script for the given SCID and exits
// early with an error if the provided quit channel is closed before
// completion.
func FetchPKScriptWithQuit(chain BlockChainIO, chanID lnwire.ShortChannelID,
	quit chan struct{}) ([]byte, error) {

	tx, err := FetchFundingTxWrapper(chain, chanID, quit)
	if err != nil {
		return nil, err
	}

	outputLocator := chanvalidate.ShortChanIDChanLocator{
		ID: chanID,
	}

	output, _, err := outputLocator.Locate(tx)
	if err != nil {
		return nil, err
	}

	return output.PkScript, nil
}
