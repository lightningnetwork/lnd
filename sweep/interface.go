package sweep

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
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

	// CheckMempoolAcceptance checks whether a transaction follows mempool
	// policies and returns an error if it cannot be accepted into the
	// mempool.
	CheckMempoolAcceptance(tx *wire.MsgTx) error

	// GetTransactionDetails returns a detailed description of a tx given
	// its transaction hash.
	GetTransactionDetails(txHash *chainhash.Hash) (
		*lnwallet.TransactionDetail, error)

	// BackEnd returns a name for the wallet's backing chain service,
	// which could be e.g. btcd, bitcoind, neutrino, or another consensus
	// service.
	BackEnd() string
}

// SweepOutput is an output used to sweep funds from a channel output.
type SweepOutput struct { //nolint:revive
	wire.TxOut

	// IsExtra indicates whether this output is an extra output that was
	// added by a party other than the sweeper.
	IsExtra bool

	// InternalKey is the taproot internal key of the extra output. This is
	// None, if this isn't a taproot output.
	InternalKey fn.Option[keychain.KeyDescriptor]
}

// AuxNotifyOpts contains options for the NotifyBroadcast call on the
// AuxSweeper interface.
type AuxNotifyOpts struct {
	// SkipBroadcast indicates whether the transaction is already
	// confirmed on-chain (true for breach sweeps, force close
	// commitments) and should not be broadcast again.
	SkipBroadcast bool

	// SkipProofVerify indicates whether aux-level proof
	// verification should be skipped. This is used when the input
	// proofs contain placeholder witnesses (e.g. second-level HTLC
	// outputs) that cannot pass VM-level validation, and the
	// on-chain confirmation serves as proof of validity instead.
	SkipProofVerify bool

	// ConfirmHeight is an optional confirmation height hint for the
	// transaction. When set, the porter uses this as the height hint
	// when scanning for the on-chain confirmation instead of the
	// current chain tip. This is critical for breach justice sweeps
	// where NotifyBroadcast is called after the tx has already been
	// confirmed and the chain has advanced past the confirmation
	// block.
	ConfirmHeight uint32

	// LookupInputProofs indicates that the aux sweeper should look
	// up the input proofs from its proof archive rather than using
	// the proofs embedded in the resolution blob. This is needed
	// when the resolution blob carries a stale proof (e.g. the
	// commit-level proof for a second-level HTLC output that has
	// since been imported with a proper second-level proof).
	LookupInputProofs bool
}

// AuxSweeper is used to enable a 3rd party to further shape the sweeping
// transaction by adding a set of extra outputs to the sweeping transaction.
type AuxSweeper interface {
	// DeriveSweepAddr takes a set of inputs, and the change address we'd
	// use to sweep them, and maybe results an extra sweep output that we
	// should add to the sweeping transaction.
	DeriveSweepAddr(inputs []input.Input,
		change lnwallet.AddrWithKey) fn.Result[SweepOutput]

	// ExtraBudgetForInputs is used to determine the extra budget that
	// should be allocated to sweep the given set of inputs. This can be
	// used to add extra funds to the sweep transaction, for example to
	// cover fees for additional outputs of custom channels.
	ExtraBudgetForInputs(inputs []input.Input) fn.Result[btcutil.Amount]

	// NotifyBroadcast is used to notify external callers of the broadcast
	// of a sweep transaction, generated by the passed BumpRequest.
	NotifyBroadcast(req *BumpRequest, tx *wire.MsgTx,
		totalFees btcutil.Amount,
		outpointToTxIndex map[wire.OutPoint]int,
		opts AuxNotifyOpts) error
}
