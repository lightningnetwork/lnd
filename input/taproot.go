package input

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// NewTxSigHashesV0Only returns a new txscript.TxSigHashes instance that will
// only calculate the sighash midstate values for segwit v0 inputs and can
// therefore never be used for transactions that want to spend segwit v1
// (taproot) inputs.
func NewTxSigHashesV0Only(tx *wire.MsgTx) *txscript.TxSigHashes {
	// The canned output fetcher returns a wire.TxOut instance with the
	// given pk script and amount. We can get away with nil since the first
	// thing the TxSigHashes constructor checks is the length of the pk
	// script and whether it matches taproot output script length. If the
	// length doesn't match it assumes v0 inputs only.
	nilFetcher := txscript.NewCannedPrevOutputFetcher(nil, 0)
	return txscript.NewTxSigHashes(tx, nilFetcher)
}

// MultiPrevOutFetcher returns a txscript.MultiPrevOutFetcher for the given set
// of inputs.
func MultiPrevOutFetcher(inputs []Input) (*txscript.MultiPrevOutFetcher, error) {
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for _, inp := range inputs {
		op := inp.OutPoint()
		desc := inp.SignDesc()

		if op == nil {
			return nil, fmt.Errorf("missing input outpoint")
		}

		if desc == nil || desc.Output == nil {
			return nil, fmt.Errorf("missing input utxo information")
		}

		fetcher.AddPrevOut(*op, desc.Output)
	}

	return fetcher, nil
}
