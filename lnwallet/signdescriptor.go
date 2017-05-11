package lnwallet

import (
	"encoding/binary"
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

// SignDescriptor houses the necessary information required to successfully sign
// a given output. This struct is used by the Signer interface in order to gain
// access to critical data needed to generate a valid signature.
type SignDescriptor struct {
	// Pubkey is the public key to which the signature should be generated
	// over. The Signer should then generate a signature with the private
	// key corresponding to this public key.
	PubKey *btcec.PublicKey

	// PrivateTweak is a scalar value that should be added to the private
	// key corresponding to the above public key to obtain the private key
	// to be used to sign this input. This value is typically a leaf node
	// from the revocation tree.
	//
	// NOTE: If this value is nil, then the input can be signed using only
	// the above public key.
	PrivateTweak []byte

	// WitnessScript is the full script required to properly redeem the
	// output. This field will only be populated if a p2wsh or a p2sh
	// output is being signed.
	WitnessScript []byte

	// Output is the target output which should be signed. The PkScript and
	// Value fields within the output should be properly populated,
	// otherwise an invalid signature may be generated.
	Output *wire.TxOut

	// HashType is the target sighash type that should be used when
	// generating the final sighash, and signature.
	HashType txscript.SigHashType

	// SigHashes is the pre-computed sighash midstate to be used when
	// generating the final sighash for signing.
	SigHashes *txscript.TxSigHashes

	// InputIndex is the target input within the transaction that should be
	// signed.
	InputIndex int
}

// WriteSignDescriptor serializes a SignDescriptor struct into the passed
// io.Writer stream.
// NOTE: We assume the SigHashes and InputIndex fields haven't been assigned
// yet, since that is usually done just before broadcast by the witness
// generator.
func WriteSignDescriptor(w io.Writer, sd *SignDescriptor) error {
	serializedPubKey := sd.PubKey.SerializeCompressed()
	if err := wire.WriteVarBytes(w, 0, serializedPubKey); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, sd.PrivateTweak); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, sd.WitnessScript); err != nil {
		return err
	}

	if err := lnwire.WriteTxOut(w, sd.Output); err != nil {
		return err
	}

	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], uint32(sd.HashType))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return nil
}

// ReadSignDescriptor deserializes a SignDescriptor struct from the passed
// io.Reader stream.
func ReadSignDescriptor(r io.Reader, sd *SignDescriptor) error {

	pubKeyBytes, err := wire.ReadVarBytes(r, 0, 34, "pubkey")
	if err != nil {
		return err
	}
	sd.PubKey, err = btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	privateTweak, err := wire.ReadVarBytes(r, 0, 32, "privateTweak")
	if err != nil {
		return err
	}

	// Serializing a SignDescriptor with a nil-valued PrivateTweak results in
	// deserializing a zero-length slice. Since a nil-valued PrivateTweak has
	// special meaning and a zero-length slice for a PrivateTweak is invalid,
	// we can use the zero-length slice as the flag for a nil-valued
	// PrivateTweak.
	if len(privateTweak) == 0 {
		sd.PrivateTweak = nil
	} else {
		sd.PrivateTweak = privateTweak
	}

	witnessScript, err := wire.ReadVarBytes(r, 0, 100, "witnessScript")
	if err != nil {
		return err
	}
	sd.WitnessScript = witnessScript

	txOut := &wire.TxOut{}
	if err := lnwire.ReadTxOut(r, txOut); err != nil {
		return err
	}
	sd.Output = txOut

	var hashType [4]byte
	if _, err := io.ReadFull(r, hashType[:]); err != nil {
		return err
	}
	sd.HashType = txscript.SigHashType(binary.BigEndian.Uint32(hashType[:]))

	return nil
}
