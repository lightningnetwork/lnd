package sweep

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// Input contains all data needed to construct a sweep tx input.
type Input interface {
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

	// BlocksToMaturity returns the relative timelock, as a number of
	// blocks, that must be built on top of the confirmation height before
	// the output can be spent. For non-CSV locked inputs this is always
	// zero.
	BlocksToMaturity() uint32
}

// BaseInput contains all the information needed to sweep an output.
type BaseInput struct {
	outpoint    wire.OutPoint
	witnessType lnwallet.WitnessType
	signDesc    lnwallet.SignDescriptor
}

// MakeBaseInput assembles a new BaseInput that can be used to construct a
// sweep transaction.
func MakeBaseInput(outpoint *wire.OutPoint,
	witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor) BaseInput {

	return BaseInput{
		outpoint:    *outpoint,
		witnessType: witnessType,
		signDesc:    *signDescriptor,
	}
}

// OutPoint returns the breached output's identifier that is to be included as a
// transaction input.
func (bi *BaseInput) OutPoint() *wire.OutPoint {
	return &bi.outpoint
}

// WitnessType returns the type of witness that must be generated to spend the
// breached output.
func (bi *BaseInput) WitnessType() lnwallet.WitnessType {
	return bi.witnessType
}

// SignDesc returns the breached output's SignDescriptor, which is used during
// signing to compute the witness.
func (bi *BaseInput) SignDesc() *lnwallet.SignDescriptor {
	return &bi.signDesc
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. It does so by generating the witness generation function,
// which is parameterized primarily by the witness type and sign descriptor. The
// method then returns the witness computed by invoking this function.
func (bi *BaseInput) BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) ([][]byte, error) {

	witnessFunc := bi.witnessType.GenWitnessFunc(
		signer, bi.SignDesc(),
	)

	return witnessFunc(txn, hashCache, txinIdx)
}

// BlocksToMaturity returns the relative timelock, as a number of blocks, that
// must be built on top of the confirmation height before the output can be
// spent. For non-CSV locked inputs this is always zero.
func (bi *BaseInput) BlocksToMaturity() uint32 {
	return 0
}

// Add compile-time constraint ensuring BaseInput implements
// SpendableOutput.
var _ Input = (*BaseInput)(nil)
