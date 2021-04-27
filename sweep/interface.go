package sweep

import (
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
}
