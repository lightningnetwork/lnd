package input

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// ErrTweakOverdose signals a SignDescriptor is invalid because both of its
	// SingleTweak and DoubleTweak are non-nil.
	ErrTweakOverdose = errors.New("sign descriptor should only have one tweak")
)

// SignDescriptor houses the necessary information required to successfully
// sign a given segwit output. This struct is used by the Signer interface in
// order to gain access to critical data needed to generate a valid signature.
type SignDescriptor struct {
	// KeyDesc is a descriptor that precisely describes *which* key to use
	// for signing. This may provide the raw public key directly, or
	// require the Signer to re-derive the key according to the populated
	// derivation path.
	KeyDesc keychain.KeyDescriptor

	// SingleTweak is a scalar value that will be added to the private key
	// corresponding to the above public key to obtain the private key to
	// be used to sign this input. This value is typically derived via the
	// following computation:
	//
	//  * derivedKey = privkey + sha256(perCommitmentPoint || pubKey) mod N
	//
	// NOTE: If this value is nil, then the input can be signed using only
	// the above public key. Either a SingleTweak should be set or a
	// DoubleTweak, not both.
	SingleTweak []byte

	// DoubleTweak is a private key that will be used in combination with
	// its corresponding private key to derive the private key that is to
	// be used to sign the target input. Within the Lightning protocol,
	// this value is typically the commitment secret from a previously
	// revoked commitment transaction. This value is in combination with
	// two hash values, and the original private key to derive the private
	// key to be used when signing.
	//
	//  * k = (privKey*sha256(pubKey || tweakPub) +
	//        tweakPriv*sha256(tweakPub || pubKey)) mod N
	//
	// NOTE: If this value is nil, then the input can be signed using only
	// the above public key. Either a SingleTweak should be set or a
	// DoubleTweak, not both.
	DoubleTweak *btcec.PrivateKey

	// TapTweak is a 32-byte value that will be used to derive a taproot
	// output public key (or the corresponding private key) from an
	// internal key and this tweak. The transformation applied is:
	//  * outputKey = internalKey +
	//        tagged_hash("tapTweak", internalKey || tapTweak)
	//
	// When attempting to sign an output derived via BIP 86, then this
	// field should be an empty byte array.
	//
	// When attempting to sign for the key spend path of an output key that
	// commits to an actual script tree, the script root should be used.
	TapTweak []byte

	// WitnessScript is the full script required to properly redeem the
	// output. This field should be set to the full script if a p2wsh
	// output is being signed. For p2wkh it should be set to the hashed
	// script (PkScript).
	WitnessScript []byte

	// SignMethod specifies how the input should be signed. Depending on the
	// selected method, either the TapTweak, WitnessScript or both need to
	// be specified.
	SignMethod SignMethod

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

	// PrevOutputFetcher is an interface that can return the output
	// information on all UTXOs that are being spent in this transaction.
	// This MUST be set when spending Taproot outputs.
	PrevOutputFetcher txscript.PrevOutputFetcher

	// ControlBlock is a fully serialized control block that contains the
	// merkle proof necessary to spend a taproot output. This may
	// optionally be set if the SignMethod is
	// input.TaprootScriptSpendSignMethod. In which case, this should be an
	// inclusion proof for the WitnessScript.
	ControlBlock []byte

	// InputIndex is the target input within the transaction that should be
	// signed.
	InputIndex int
}

// SignMethod defines the different ways a signer can sign, given a specific
// input.
type SignMethod uint8

const (
	// WitnessV0SignMethod denotes that a SegWit v0 (p2wkh, np2wkh, p2wsh)
	// input script should be signed.
	WitnessV0SignMethod SignMethod = 0

	// TaprootKeySpendBIP0086SignMethod denotes that a SegWit v1 (p2tr)
	// input should be signed by using the BIP0086 method (commit to
	// internal key only).
	TaprootKeySpendBIP0086SignMethod SignMethod = 1

	// TaprootKeySpendSignMethod denotes that a SegWit v1 (p2tr)
	// input should be signed by using a given taproot hash to commit to in
	// addition to the internal key.
	TaprootKeySpendSignMethod SignMethod = 2

	// TaprootScriptSpendSignMethod denotes that a SegWit v1 (p2tr) input
	// should be spent using the script path and that a specific leaf script
	// should be signed for.
	TaprootScriptSpendSignMethod SignMethod = 3
)

// String returns a human-readable representation of the signing method.
func (s SignMethod) String() string {
	switch s {
	case WitnessV0SignMethod:
		return "witness_v0"
	case TaprootKeySpendBIP0086SignMethod:
		return "taproot_key_spend_bip86"
	case TaprootKeySpendSignMethod:
		return "taproot_key_spend"
	case TaprootScriptSpendSignMethod:
		return "taproot_script_spend"
	default:
		return fmt.Sprintf("unknown<%d>", s)
	}
}

// PkScriptCompatible returns true if the given public key script is compatible
// with the sign method.
func (s SignMethod) PkScriptCompatible(pkScript []byte) bool {
	switch s {
	// SegWit v0 can be p2wkh, np2wkh, p2wsh.
	case WitnessV0SignMethod:
		return txscript.IsPayToWitnessPubKeyHash(pkScript) ||
			txscript.IsPayToWitnessScriptHash(pkScript) ||
			txscript.IsPayToScriptHash(pkScript)

	case TaprootKeySpendBIP0086SignMethod, TaprootKeySpendSignMethod,
		TaprootScriptSpendSignMethod:

		return txscript.IsPayToTaproot(pkScript)

	default:
		return false
	}
}

// WriteSignDescriptor serializes a SignDescriptor struct into the passed
// io.Writer stream.
//
// NOTE: We assume the SigHashes and InputIndex fields haven't been assigned
// yet, since that is usually done just before broadcast by the witness
// generator.
func WriteSignDescriptor(w io.Writer, sd *SignDescriptor) error {
	err := binary.Write(w, binary.BigEndian, sd.KeyDesc.Family)
	if err != nil {
		return err
	}
	err = binary.Write(w, binary.BigEndian, sd.KeyDesc.Index)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, sd.KeyDesc.PubKey != nil)
	if err != nil {
		return err
	}

	if sd.KeyDesc.PubKey != nil {
		serializedPubKey := sd.KeyDesc.PubKey.SerializeCompressed()
		if err := wire.WriteVarBytes(w, 0, serializedPubKey); err != nil {
			return err
		}
	}

	if err := wire.WriteVarBytes(w, 0, sd.SingleTweak); err != nil {
		return err
	}

	var doubleTweakBytes []byte
	if sd.DoubleTweak != nil {
		doubleTweakBytes = sd.DoubleTweak.Serialize()
	}
	if err := wire.WriteVarBytes(w, 0, doubleTweakBytes); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, sd.WitnessScript); err != nil {
		return err
	}

	if err := writeTxOut(w, sd.Output); err != nil {
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
	err := binary.Read(r, binary.BigEndian, &sd.KeyDesc.Family)
	if err != nil {
		return err
	}
	err = binary.Read(r, binary.BigEndian, &sd.KeyDesc.Index)
	if err != nil {
		return err
	}

	var hasKey bool
	err = binary.Read(r, binary.BigEndian, &hasKey)
	if err != nil {
		return err
	}

	if hasKey {
		pubKeyBytes, err := wire.ReadVarBytes(r, 0, 34, "pubkey")
		if err != nil {
			return err
		}
		sd.KeyDesc.PubKey, err = btcec.ParsePubKey(pubKeyBytes)
		if err != nil {
			return err
		}
	}

	singleTweak, err := wire.ReadVarBytes(r, 0, 32, "singleTweak")
	if err != nil {
		return err
	}

	// Serializing a SignDescriptor with a nil-valued SingleTweak results
	// in deserializing a zero-length slice. Since a nil-valued SingleTweak
	// has special meaning and a zero-length slice for a SingleTweak is
	// invalid, we can use the zero-length slice as the flag for a
	// nil-valued SingleTweak.
	if len(singleTweak) == 0 {
		sd.SingleTweak = nil
	} else {
		sd.SingleTweak = singleTweak
	}

	doubleTweakBytes, err := wire.ReadVarBytes(r, 0, 32, "doubleTweak")
	if err != nil {
		return err
	}

	// Serializing a SignDescriptor with a nil-valued DoubleTweak results
	// in deserializing a zero-length slice. Since a nil-valued DoubleTweak
	// has special meaning and a zero-length slice for a DoubleTweak is
	// invalid, we can use the zero-length slice as the flag for a
	// nil-valued DoubleTweak.
	if len(doubleTweakBytes) == 0 {
		sd.DoubleTweak = nil
	} else {
		sd.DoubleTweak, _ = btcec.PrivKeyFromBytes(doubleTweakBytes)
	}

	// Only one tweak should ever be set, fail if both are present.
	if sd.SingleTweak != nil && sd.DoubleTweak != nil {
		return ErrTweakOverdose
	}

	witnessScript, err := wire.ReadVarBytes(r, 0, 500, "witnessScript")
	if err != nil {
		return err
	}
	sd.WitnessScript = witnessScript

	txOut := &wire.TxOut{}
	if err := readTxOut(r, txOut); err != nil {
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
