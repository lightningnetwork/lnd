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

	// SignDesc returns a reference to a spendable output's sign
	// descriptor, which is used during signing to compute a valid witness
	// that spends this output.
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

	// HeightHint returns the minimum height at which a confirmed spending
	// tx can occur.
	HeightHint() uint32
}

type inputKit struct {
	outpoint    wire.OutPoint
	witnessType lnwallet.WitnessType
	signDesc    lnwallet.SignDescriptor
	heightHint  uint32
}

// OutPoint returns the breached output's identifier that is to be included as
// a transaction input.
func (i *inputKit) OutPoint() *wire.OutPoint {
	return &i.outpoint
}

// WitnessType returns the type of witness that must be generated to spend the
// breached output.
func (i *inputKit) WitnessType() lnwallet.WitnessType {
	return i.witnessType
}

// SignDesc returns the breached output's SignDescriptor, which is used during
// signing to compute the witness.
func (i *inputKit) SignDesc() *lnwallet.SignDescriptor {
	return &i.signDesc
}

// HeightHint returns the minimum height at which a confirmed spending
// tx can occur.
func (i *inputKit) HeightHint() uint32 {
	return i.heightHint
}

// BaseInput contains all the information needed to sweep a basic output
// (CSV/CLTV/no time lock)
type BaseInput struct {
	inputKit
}

// MakeBaseInput assembles a new BaseInput that can be used to construct a
// sweep transaction.
func MakeBaseInput(outpoint *wire.OutPoint, witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor, heightHint uint32) BaseInput {

	return BaseInput{
		inputKit{
			outpoint:    *outpoint,
			witnessType: witnessType,
			signDesc:    *signDescriptor,
			heightHint:  heightHint,
		},
	}
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. It does so by generating the witness generation function,
// which is parameterized primarily by the witness type and sign descriptor.
// The method then returns the witness computed by invoking this function.
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

// HtlcSucceedInput constitutes a sweep input that needs a pre-image. The input
// is expected to reside on the commitment tx of the remote party and should
// not be a second level tx output.
type HtlcSucceedInput struct {
	inputKit

	preimage []byte
}

// MakeHtlcSucceedInput assembles a new redeem input that can be used to
// construct a sweep transaction.
func MakeHtlcSucceedInput(outpoint *wire.OutPoint,
	signDescriptor *lnwallet.SignDescriptor,
	preimage []byte, heightHint uint32) HtlcSucceedInput {

	return HtlcSucceedInput{
		inputKit: inputKit{
			outpoint:    *outpoint,
			witnessType: lnwallet.HtlcAcceptedRemoteSuccess,
			signDesc:    *signDescriptor,
			heightHint:  heightHint,
		},
		preimage: preimage,
	}
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. For HtlcSpendInput it will need to make the preimage part
// of the witness.
func (h *HtlcSucceedInput) BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) ([][]byte, error) {

	desc := h.signDesc
	desc.SigHashes = hashCache
	desc.InputIndex = txinIdx

	return lnwallet.SenderHtlcSpendRedeem(
		signer, &desc, txn,
		h.preimage,
	)
}

// BlocksToMaturity returns the relative timelock, as a number of blocks, that
// must be built on top of the confirmation height before the output can be
// spent.
func (h *HtlcSucceedInput) BlocksToMaturity() uint32 {
	return 0
}

// Compile-time constraints to ensure each input struct implement the Input
// interface.
var _ Input = (*BaseInput)(nil)
var _ Input = (*HtlcSucceedInput)(nil)
