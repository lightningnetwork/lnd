package sweep

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// SpendableOutput an interface which can be used by the breach arbiter to
// construct a transaction spending from outputs we control.
type SpendableOutput interface {
	// Amount returns the number of satoshis contained within the output.
	Amount() btcutil.Amount

	// Outpoint returns the reference to the output being spent, used to
	// construct the corresponding transaction input.
	OutPoint() *wire.OutPoint

	// WitnessType returns an enum specifying the type of witness that must
	// be generated in order to spend this output.
	WitnessType() lnwallet.WitnessType

	// SignDesc returns a reference to a spendable output's sign descriptor,
	// which is used during signing to compute a valid witness that spends
	// this output.
	SignDesc() *lnwallet.SignDescriptor

	// BuildWitness returns a valid witness allowing this output to be
	// spent, the witness should be attached to the transaction at the
	// location determined by the given `txinIdx`.
	BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
		hashCache *txscript.TxSigHashes,
		txinIdx int) ([][]byte, error)
}

// CsvSpendableOutput is a SpendableOutput that contains all of the information
// necessary to construct, sign, and sweep an output locked with a CSV delay.
type CsvSpendableOutput interface {
	SpendableOutput

	// ConfHeight returns the height at which this output was confirmed.
	// A zero value indicates that the output has not been confirmed.
	ConfHeight() uint32

	// SetConfHeight marks the height at which the output is confirmed in
	// the chain.
	SetConfHeight(height uint32)

	// BlocksToMaturity returns the relative timelock, as a number of
	// blocks, that must be built on top of the confirmation height before
	// the output can be spent.
	BlocksToMaturity() uint32

	// OriginChanPoint returns the outpoint of the channel from which this
	// output is derived.
	OriginChanPoint() *wire.OutPoint
}
