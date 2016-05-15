package lnwallet

import (
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// WalletController defines an abstract interface for controlling a local Pure
// Go wallet, a local or remote wallet via an RPC mechanism, or possibly even
// a daemon assisted hardware wallet. This interface serves the purpose of
// allowing LightningWallet to be seamlessly compatible with several wallets
// such as: uspv, btcwallet, Bitcoin Core, Electrum, etc. This interface then
// serves as a "base wallet", with Lightning Network awareness taking place at
// a "higher" level of abstraction. Essentially, an overlay wallet.
// Implementors of this interface must closely adhere to the documented behavior
// of all interface methods in order to ensure identical behavior accross all
// concrete implementations.
type WalletController interface {
	// ConfirmedBalance returns the sum of all the wallet's unspent outputs
	// that have at least confs confirmations. If confs is set to zero,
	// then all unspent outputs, including those currently in the mempool
	// will be included in the final sum.
	ConfirmedBalance(confs int32) btcutil.Amount

	// NewAddress returns the next external address for the wallet. The
	// type of address returned is dictated by the wallet's capabilities,
	// and may be of type: p2sh, p2pkh, p2wkh, p2wsh, etc.
	NewAddress(witness bool) (btcutil.Address, error)

	// NewChangeAddress returns a new change address for the wallet. If the
	// underlying wallet supports hd key chains, then this address should be
	// dervied from an internal branch.
	NewChangeAddress(witness bool) (btcutil.Address, error)

	// GetPrivKey retrives the underlying private key associated with the
	// passed address. If the wallet is unable to locate this private key
	// due to the address not being under control of the wallet, then an
	// error should be returned.
	GetPrivKey(a *btcutil.Address) (*btcec.PrivateKey, error)

	// NewRawKey returns a raw private key controlled by the wallet. These
	// keys are used for the 2-of-2 multi-sig outputs for funding
	// transactions, as well as the pub key used for commitment transactions.
	// TODO(roasbeef): key pool due to cancelled reservations??
	NewRawKey() (*btcec.PrivateKey, error)

	// FetchIdentityKey returns a private key which will be utilized as the
	// wallet's Lightning Network identity for authentication purposes.
	// TODO(roasbeef): rotate identity key?
	FetchIdentityKey() (*btcec.PrivateKey, error)

	// FundTransaction creates a new unsigned transactions paying to the
	// passed outputs, possibly using the specified change address. The
	// includeFee parameter dictates if the wallet should also provide
	// enough the funds necessary to create an adequate fee or not.
	FundTransaction(outputs []*wire.TxOut, changeAddr btcutil.Address,
		includeFee bool) (*wire.MsgTx, error)

	// SignTransaction performs potentially a sparse, or full signing of
	// all inputs within the passed transaction that are spendable by the
	// wallet.
	SignTransaction(tx *wire.MsgTx) error

	// BroadcastTransaction performs cursory validation (dust checks, etc),
	// then finally broadcasts the passed transaction to the Bitcoin network.
	BroadcastTransaction(tx *wire.MsgTx) error

	// SendMany funds, signs, and broadcasts a Bitcoin transaction paying
	// out to the specified outputs. In the case the wallet has insufficient
	// funds, or the outputs are non-standard, and error should be returned.
	SendMany(outputs []*wire.TxOut) (*wire.ShaHash, error)

	// ListUnspentWitness returns all unspent outputs which are version 0
	// witness programs. The 'confirms' parameter indicates the minimum
	// number of confirmations an output needs in order to be returned by
	// this method. Passing -1 as 'confirms' indicates that even unconfirmed
	// outputs should be returned.
	ListUnspentWitness(confirms int32) ([]*wire.OutPoint, error)

	// LockOutpoint marks an outpoint as locked meaning it will no longer
	// be deemed as eligble for coin selection. Locking outputs are utilized
	// in order to avoid race conditions when selecting inputs for usage when
	// funding a channel.
	LockOutpoint(o wire.OutPoint)

	// UnlockOutpoint unlocks an previously locked output, marking it
	// eligible for coin seleciton.
	UnlockOutpoint(o wire.OutPoint)

	// ImportScript imports the serialize public key script, or redeem
	// script into the wallet's database. Scripts to be imported include
	// the 2-of-2 script for funding transactions, commitment scripts,
	// HTLCs scripts, and so on.
	ImportScript(b []byte) error

	// Start initializes the wallet, making any neccessary connections,
	// starting up required goroutines etc.
	Start() error

	// Stop signals the wallet for shutdown. Shutdown may entail closing
	// any active sockets, database handles, stopping goroutines, etc.
	Stop() error

	// WaitForShutdown blocks until the wallet finishes the shutdown
	// procedure triggered by a prior call to Stop().
	WaitForShutdown() error

	// TODO(roasbeef): ImportPriv?
	//  * segwitty flag?
}
