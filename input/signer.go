package input

import (
	"github.com/btcsuite/btcd/wire"
)

// Signer represents an abstract object capable of generating raw signatures as
// well as full complete input scripts given a valid SignDescriptor and
// transaction. This interface fully abstracts away signing paving the way for
// Signer implementations such as hardware wallets, hardware tokens, HSM's, or
// simply a regular wallet.
type Signer interface {
	// SignOutputRaw generates a signature for the passed transaction
	// according to the data within the passed SignDescriptor.
	//
	// NOTE: The resulting signature should be void of a sighash byte.
	SignOutputRaw(tx *wire.MsgTx, signDesc *SignDescriptor) ([]byte, error)

	// ComputeInputScript generates a complete InputIndex for the passed
	// transaction with the signature as defined within the passed
	// SignDescriptor. This method should be capable of generating the
	// proper input script for both regular p2wkh output and p2wkh outputs
	// nested within a regular p2sh output.
	//
	// NOTE: This method will ignore any tweak parameters set within the
	// passed SignDescriptor as it assumes a set of typical script
	// templates (p2wkh, np2wkh, etc).
	ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*Script, error)
}

// Script represents any script inputs required to redeem a previous
// output. This struct is used rather than just a witness, or scripSig in order
// to accommodate nested p2sh which utilizes both types of input scripts.
type Script struct {
	// Witness is the full witness stack required to unlock this output.
	Witness wire.TxWitness

	// SigScript will only be populated if this is an input script sweeping
	// a nested p2sh output.
	SigScript []byte
}
