package sweep

import (
	"errors"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// ErrNoWitnessStack signals that the witness for a particular input was never
// computed or set properly.
var ErrNoWitnessStack = errors.New("witness is not built or set")

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
		txinIdx int) error

	// SetWitnessStack is used to set the input's witness stack if the
	// witness was computed externally. This is useful if the witness is
	// known, but the signing keys are not accessible.
	SetWitnessStack([][]byte)

	// Witness returns the inputs witness, computed either via signing or by
	// setting the witness stack directly and appending the witness script.
	Witness() ([][]byte, error)

	// BlocksToMaturity returns the relative timelock, as a number of
	// blocks, that must be built on top of the confirmation height before
	// the output can be spent. For non-CSV locked inputs this is always
	// zero.
	BlocksToMaturity() uint32
}

type inputKit struct {
	outpoint     wire.OutPoint
	witnessType  lnwallet.WitnessType
	signDesc     lnwallet.SignDescriptor
	witnessStack [][]byte
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

// BaseInput contains all the information needed to sweep a basic output
// (CSV/CLTV/no time lock)
type BaseInput struct {
	inputKit
}

// MakeBaseInput assembles a new BaseInput that can be used to construct a
// sweep transaction.
func MakeBaseInput(outpoint *wire.OutPoint, witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor) BaseInput {

	return BaseInput{
		inputKit{
			outpoint:    *outpoint,
			witnessType: witnessType,
			signDesc:    *signDescriptor,
		},
	}
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. It does so by generating the witness generation function,
// which is parameterized primarily by the witness type and sign descriptor.
// The method then returns the witness computed by invoking this function.
func (bi *BaseInput) BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) error {

	witnessFunc := bi.witnessType.GenWitnessFunc(
		signer, bi.SignDesc(),
	)

	witness, err := witnessFunc(txn, hashCache, txinIdx)
	switch {

	// If we attempted to sign a preauthorized input, we can ignore this
	// error and assume the input is already signed for. If it hasn't, the
	// error will be surfaced when calling Witness().
	case err == lnwallet.ErrSignPreAuthorized:
		return nil

	// Any other non-nil errors should be bubbled up.
	case err != nil:
		return err
	}

	bi.witnessStack = witness[:len(witness)-1]

	return nil
}

// SetWitnessStack is used to set the input's witness stack if the witness was
// computed externally. This is useful if the witness is known, but the signing
// keys are not accessible.
func (bi *BaseInput) SetWitnessStack(witnessStack [][]byte) {
	bi.witnessStack = witnessStack
}

// Witness returns the inputs witness, computed either via signing or by setting
// the witness stack directly and appending the witness script.
func (bi *BaseInput) Witness() ([][]byte, error) {
	if bi.witnessStack == nil {
		return nil, ErrNoWitnessStack
	}

	// Copy witness stack and set the last element to the witness script.
	witness := make([][]byte, len(bi.witnessStack)+1)
	witness[copy(witness, bi.witnessStack)] = bi.SignDesc().WitnessScript

	return witness, nil
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
	preimage []byte) HtlcSucceedInput {

	return HtlcSucceedInput{
		inputKit: inputKit{
			outpoint:    *outpoint,
			witnessType: lnwallet.HtlcAcceptedRemoteSuccess,
			signDesc:    *signDescriptor,
		},
		preimage: preimage,
	}
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. For HtlcSpendInput it will need to make the preimage part
// of the witness.
func (h *HtlcSucceedInput) BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) error {

	desc := h.signDesc
	desc.SigHashes = hashCache
	desc.InputIndex = txinIdx

	witness, err := lnwallet.SenderHtlcSpendRedeem(
		signer, &desc, txn, h.preimage,
	)
	if err != nil {
		return err
	}

	h.witnessStack = witness[:len(witness)-1]

	return nil
}

// SetWitnessStack is a NOP for this special cased input, it must be constructed
// via BuildWitness.
func (h *HtlcSucceedInput) SetWitnessStack(witnessStack [][]byte) {}

// Witness returns the input's witness computed via BuildWitness.
func (h *HtlcSucceedInput) Witness() ([][]byte, error) {
	if h.witnessStack == nil {
		return nil, ErrNoWitnessStack
	}

	// Copy witness stack and set the last element to the witness script.
	witness := make([][]byte, len(h.witnessStack)+1)
	witness[copy(witness, h.witnessStack)] = h.SignDesc().WitnessScript

	return witness, nil
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
