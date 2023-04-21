package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// Wallet contains all wallet related functionality required by sweeper.
type Wallet interface {
	// PublishTransaction performs cursory validation (dust checks, etc) and
	// broadcasts the passed transaction to the Bitcoin network.
	PublishTransaction(tx *wire.MsgTx, label string) error

	// ListUnspentWitnessFromDefaultAccount returns all unspent outputs
	// which are version 0 witness programs from the default wallet account.
	// The 'minConfs' and 'maxConfs' parameters indicate the minimum
	// and maximum number of confirmations an output needs in order to be
	// returned by this method.
	ListUnspentWitnessFromDefaultAccount(minConfs, maxConfs int32) (
		[]*lnwallet.Utxo, error)

	// WithCoinSelectLock will execute the passed function closure in a
	// synchronized manner preventing any coin selection operations from
	// proceeding while the closure is executing. This can be seen as the
	// ability to execute a function closure under an exclusive coin
	// selection lock.
	WithCoinSelectLock(f func() error) error

	// RemoveDescendants removes any wallet transactions that spends
	// outputs created by the specified transaction.
	RemoveDescendants(*wire.MsgTx) error

	// FetchTx returns the transaction that corresponds to the transaction
	// hash passed in. If the transaction can't be found then a nil
	// transaction pointer is returned.
	FetchTx(chainhash.Hash) (*wire.MsgTx, error)

	// CancelRebroadcast is used to inform the rebroadcaster sub-system
	// that it no longer needs to try to rebroadcast a transaction. This is
	// used to ensure that invalid transactions (inputs spent) aren't
	// retried in the background.
	CancelRebroadcast(tx chainhash.Hash)
}
