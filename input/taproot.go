package input

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

const (
	// PubKeyFormatCompressedOdd is the identifier prefix byte for a public
	// key whose Y coordinate is odd when serialized in the compressed
	// format per section 2.3.4 of
	// [SEC1](https://secg.org/sec1-v2.pdf#subsubsection.2.3.4).
	// This is copied from the github.com/decred/dcrd/dcrec/secp256k1/v4 to
	// avoid needing to directly reference (and by accident pull in
	// incompatible crypto primitives) the package.
	PubKeyFormatCompressedOdd byte = 0x03
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

// TapscriptFullTree creates a waddrmgr.Tapscript for the given internal key and
// tree leaves.
func TapscriptFullTree(internalKey *btcec.PublicKey,
	allTreeLeaves ...txscript.TapLeaf) *waddrmgr.Tapscript {

	tree := txscript.AssembleTaprootScriptTree(allTreeLeaves...)
	rootHash := tree.RootNode.TapHash()
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])

	var outputKeyYIsOdd bool
	if tapKey.SerializeCompressed()[0] == PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	return &waddrmgr.Tapscript{
		Type: waddrmgr.TapscriptTypeFullTree,
		ControlBlock: &txscript.ControlBlock{
			InternalKey:     internalKey,
			OutputKeyYIsOdd: outputKeyYIsOdd,
			LeafVersion:     txscript.BaseLeafVersion,
		},
		Leaves: allTreeLeaves,
	}
}

// TapscriptPartialReveal creates a waddrmgr.Tapscript for the given internal
// key and revealed script.
func TapscriptPartialReveal(internalKey *btcec.PublicKey,
	revealedLeaf txscript.TapLeaf,
	inclusionProof []byte) *waddrmgr.Tapscript {

	controlBlock := &txscript.ControlBlock{
		InternalKey:    internalKey,
		LeafVersion:    txscript.BaseLeafVersion,
		InclusionProof: inclusionProof,
	}
	rootHash := controlBlock.RootHash(revealedLeaf.Script)
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)

	if tapKey.SerializeCompressed()[0] == PubKeyFormatCompressedOdd {
		controlBlock.OutputKeyYIsOdd = true
	}

	return &waddrmgr.Tapscript{
		Type:           waddrmgr.TapscriptTypePartialReveal,
		ControlBlock:   controlBlock,
		RevealedScript: revealedLeaf.Script,
	}
}

// TapscriptRootHashOnly creates a waddrmgr.Tapscript for the given internal key
// and root hash.
func TapscriptRootHashOnly(internalKey *btcec.PublicKey,
	rootHash []byte) *waddrmgr.Tapscript {

	controlBlock := &txscript.ControlBlock{
		InternalKey: internalKey,
	}

	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	if tapKey.SerializeCompressed()[0] == PubKeyFormatCompressedOdd {
		controlBlock.OutputKeyYIsOdd = true
	}

	return &waddrmgr.Tapscript{
		Type:         waddrmgr.TaprootKeySpendRootHash,
		ControlBlock: controlBlock,
		RootHash:     rootHash,
	}
}

// TapscriptFullKeyOnly creates a waddrmgr.Tapscript for the given full Taproot
// key.
func TapscriptFullKeyOnly(taprootKey *btcec.PublicKey) *waddrmgr.Tapscript {
	return &waddrmgr.Tapscript{
		Type:          waddrmgr.TaprootFullKeyOnly,
		FullOutputKey: taprootKey,
	}
}
