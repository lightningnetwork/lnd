package rpcwallet

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/psbt"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

var (
	// ErrRemoteSigningPrivateKeyNotAvailable is the error that is returned
	// if an operation is requested from the RPC wallet that is not
	// supported in remote signing mode.
	ErrRemoteSigningPrivateKeyNotAvailable = errors.New("deriving " +
		"private key is not supported by RPC based key ring")
)

// RPCKeyRing is an implementation of the SecretKeyRing interface that uses a
// local watch-only wallet for keeping track of addresses and transactions but
// delegates any signing or ECDH operations to a remote node through RPC.
type RPCKeyRing struct {
	// WalletController is the embedded wallet controller of the watch-only
	// base wallet. We need to overwrite/shadow certain of the implemented
	// methods to make sure we can mirror them to the remote wallet.
	lnwallet.WalletController

	watchOnlyKeyRing keychain.SecretKeyRing

	coinType uint32

	rpcTimeout time.Duration

	signerClient signrpc.SignerClient
	walletClient walletrpc.WalletKitClient
}

var _ keychain.SecretKeyRing = (*RPCKeyRing)(nil)
var _ input.Signer = (*RPCKeyRing)(nil)
var _ keychain.MessageSignerRing = (*RPCKeyRing)(nil)
var _ lnwallet.WalletController = (*RPCKeyRing)(nil)

// NewRPCKeyRing creates a new remote signing secret key ring that uses the
// given watch-only base wallet to keep track of addresses and transactions but
// delegates any signing or ECDH operations to the remove signer through RPC.
func NewRPCKeyRing(watchOnlyKeyRing keychain.SecretKeyRing,
	watchOnlyWalletController lnwallet.WalletController,
	remoteSigner *lncfg.RemoteSigner, coinType uint32) (*RPCKeyRing, error) {

	rpcConn, err := connectRPC(
		remoteSigner.RPCHost, remoteSigner.TLSCertPath,
		remoteSigner.MacaroonPath, remoteSigner.Timeout,
	)
	if err != nil {
		return nil, fmt.Errorf("error connecting to the remote "+
			"signing node through RPC: %v", err)
	}

	return &RPCKeyRing{
		WalletController: watchOnlyWalletController,
		watchOnlyKeyRing: watchOnlyKeyRing,
		coinType:         coinType,
		rpcTimeout:       remoteSigner.Timeout,
		signerClient:     signrpc.NewSignerClient(rpcConn),
		walletClient:     walletrpc.NewWalletKitClient(rpcConn),
	}, nil
}

// NewAddress returns the next external or internal address for the
// wallet dictated by the value of the `change` parameter. If change is
// true, then an internal address should be used, otherwise an external
// address should be returned. The type of address returned is dictated
// by the wallet's capabilities, and may be of type: p2sh, p2wkh,
// p2wsh, etc. The account parameter must be non-empty as it determines
// which account the address should be generated from.
func (r *RPCKeyRing) NewAddress(addrType lnwallet.AddressType, change bool,
	account string) (btcutil.Address, error) {

	return r.WalletController.NewAddress(addrType, change, account)
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// NOTE: This is a part of the WalletController interface.
//
// NOTE: This method only signs with BIP49/84 keys.
func (r *RPCKeyRing) SendOutputs(outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, minConfs int32,
	label string) (*wire.MsgTx, error) {

	tx, err := r.WalletController.SendOutputs(
		outputs, feeRate, minConfs, label,
	)
	if err != nil && err != basewallet.ErrTxUnsigned {
		return nil, err
	}
	if err == nil {
		// This shouldn't happen since our wallet controller is watch-
		// only and can't sign the TX.
		return tx, nil
	}

	// We know at this point that we only have inputs from our own wallet.
	// So we can just compute the input script using the remote signer.
	signDesc := input.SignDescriptor{
		HashType:  txscript.SigHashAll,
		SigHashes: txscript.NewTxSigHashes(tx),
	}
	for i, txIn := range tx.TxIn {
		// We can only sign this input if it's ours, so we'll ask the
		// watch-only wallet if it can map this outpoint into a coin we
		// own. If not, then we can't continue because our wallet state
		// is out of sync.
		info, err := r.WalletController.FetchInputInfo(
			&txIn.PreviousOutPoint,
		)
		if err != nil {
			return nil, fmt.Errorf("error looking up utxo: %v", err)
		}

		// Now that we know the input is ours, we'll populate the
		// signDesc with the per input unique information.
		signDesc.Output = &wire.TxOut{
			Value:    int64(info.Value),
			PkScript: info.PkScript,
		}
		signDesc.InputIndex = i

		// Finally, we'll sign the input as is, and populate the input
		// with the witness and sigScript (if needed).
		inputScript, err := r.ComputeInputScript(tx, &signDesc)
		if err != nil {
			return nil, err
		}

		txIn.SignatureScript = inputScript.SigScript
		txIn.Witness = inputScript.Witness
	}

	return tx, r.WalletController.PublishTransaction(tx, label)
}

// SignPsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all unsigned inputs that have all required fields
// (UTXO information, BIP32 derivation information, witness or sig scripts) set.
// If no error is returned, the PSBT is ready to be given to the next signer or
// to be finalized if lnd was the last signer.
//
// NOTE: This RPC only signs inputs (and only those it can sign), it does not
// perform any other tasks (such as coin selection, UTXO locking or
// input/output/fee value validation, PSBT finalization). Any input that is
// incomplete will be skipped.
func (r *RPCKeyRing) SignPsbt(packet *psbt.Packet) error {
	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return fmt.Errorf("error serializing PSBT: %v", err)
	}

	resp, err := r.walletClient.SignPsbt(ctxt, &walletrpc.SignPsbtRequest{
		FundedPsbt: buf.Bytes(),
	})
	if err != nil {
		err = fmt.Errorf("error signing PSBT in remote signer "+
			"instance: %v", err)

		// Log as critical as we should shut down if there is no signer.
		log.Criticalf("RPC signer error: %v", err)
		return err
	}

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(resp.SignedPsbt), false,
	)
	if err != nil {
		return fmt.Errorf("error parsing signed PSBT: %v", err)
	}

	// The caller expects the packet to be modified instead of a new
	// instance to be returned. So we just overwrite all fields in the
	// original packet.
	packet.UnsignedTx = signedPacket.UnsignedTx
	packet.Inputs = signedPacket.Inputs
	packet.Outputs = signedPacket.Outputs
	packet.Unknowns = signedPacket.Unknowns

	return nil
}

// FinalizePsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all inputs that belong to the specified account.
// Lnd must be the last signer of the transaction. That means, if there are any
// unsigned non-witness inputs or inputs without UTXO information attached or
// inputs without witness data that do not belong to lnd's wallet, this method
// will fail. If no error is returned, the PSBT is ready to be extracted and the
// final TX within to be broadcast.
//
// NOTE: This method does NOT publish the transaction after it's been
// finalized successfully.
//
// NOTE: This is a part of the WalletController interface.
//
// NOTE: We need to overwrite this method because we need to redirect the call
// to ComputeInputScript to the RPC key ring's implementation. If we forward
// the call to the default WalletController implementation, we get an error
// since that wallet is watch-only. If we forward the call to the remote signer,
// we get an error because the signer doesn't know the UTXO information required
// in ComputeInputScript.
//
// TODO(guggero): Refactor btcwallet to accept ComputeInputScript as a function
// parameter in FinalizePsbt so we can get rid of this code duplication.
func (r *RPCKeyRing) FinalizePsbt(packet *psbt.Packet, _ string) error {
	// Let's check that this is actually something we can and want to sign.
	// We need at least one input and one output.
	err := psbt.VerifyInputOutputLen(packet, true, true)
	if err != nil {
		return err
	}

	// Go through each input that doesn't have final witness data attached
	// to it already and try to sign it. We do expect that we're the last
	// ones to sign. If there is any input without witness data that we
	// cannot sign because it's not our UTXO, this will be a hard failure.
	tx := packet.UnsignedTx
	sigHashes := txscript.NewTxSigHashes(tx)
	for idx, txIn := range tx.TxIn {
		in := packet.Inputs[idx]

		// We can only sign if we have UTXO information available. We
		// can just continue here as a later step will fail with a more
		// precise error message.
		if in.WitnessUtxo == nil && in.NonWitnessUtxo == nil {
			continue
		}

		// Skip this input if it's got final witness data attached.
		if len(in.FinalScriptWitness) > 0 {
			continue
		}

		// We can only sign this input if it's ours, so we try to map it
		// to a coin we own. If we can't, then we'll continue as it
		// isn't our input.
		utxo, err := r.FetchInputInfo(&txIn.PreviousOutPoint)
		if err != nil {
			continue
		}
		fullTx := utxo.PrevTx
		signDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{},
			Output: &wire.TxOut{
				Value:    int64(utxo.Value),
				PkScript: utxo.PkScript,
			},
			HashType:   in.SighashType,
			SigHashes:  sigHashes,
			InputIndex: idx,
		}

		// Find out what UTXO we are signing. Wallets _should_ always
		// provide the full non-witness UTXO for segwit v0.
		var signOutput *wire.TxOut
		if in.NonWitnessUtxo != nil {
			prevIndex := txIn.PreviousOutPoint.Index
			signOutput = in.NonWitnessUtxo.TxOut[prevIndex]

			if !psbt.TxOutsEqual(signDesc.Output, signOutput) {
				return fmt.Errorf("found UTXO %#v but it "+
					"doesn't match PSBT's input %v",
					signDesc.Output, signOutput)
			}

			if fullTx.TxHash() != txIn.PreviousOutPoint.Hash {
				return fmt.Errorf("found UTXO tx %v but it "+
					"doesn't match PSBT's input %v",
					fullTx.TxHash(),
					txIn.PreviousOutPoint.Hash)
			}
		}

		// Fall back to witness UTXO only for older wallets.
		if in.WitnessUtxo != nil {
			signOutput = in.WitnessUtxo

			if !psbt.TxOutsEqual(signDesc.Output, signOutput) {
				return fmt.Errorf("found UTXO %#v but it "+
					"doesn't match PSBT's input %v",
					signDesc.Output, signOutput)
			}
		}

		// Do the actual signing in ComputeInputScript which in turn
		// will invoke the remote signer.
		script, err := r.ComputeInputScript(tx, signDesc)
		if err != nil {
			return fmt.Errorf("error computing input script for "+
				"input %d: %v", idx, err)
		}

		// Serialize the witness format from the stack representation to
		// the wire representation.
		var witnessBytes bytes.Buffer
		err = psbt.WriteTxWitness(&witnessBytes, script.Witness)
		if err != nil {
			return fmt.Errorf("error serializing witness: %v", err)
		}
		packet.Inputs[idx].FinalScriptWitness = witnessBytes.Bytes()
		packet.Inputs[idx].FinalScriptSig = script.SigScript
	}

	// Make sure the PSBT itself thinks it's finalized and ready to be
	// broadcast.
	err = psbt.MaybeFinalizeAll(packet)
	if err != nil {
		return fmt.Errorf("error finalizing PSBT: %v", err)
	}

	return nil
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This method is part of the keychain.KeyRing interface.
func (r *RPCKeyRing) DeriveNextKey(
	keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return r.watchOnlyKeyRing.DeriveNextKey(keyFam)
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This method is part of the keychain.KeyRing interface.
func (r *RPCKeyRing) DeriveKey(
	keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	return r.watchOnlyKeyRing.DeriveKey(keyLoc)
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// target key descriptor and remote public key. The output returned will be the
// sha256 of the resulting shared point serialized in compressed format. If k is
// our private key, and P is the public key, we perform the following operation:
//
//  sx := k*P
//  s := sha256(sx.SerializeCompressed())
//
// NOTE: This method is part of the keychain.ECDHRing interface.
func (r *RPCKeyRing) ECDH(keyDesc keychain.KeyDescriptor,
	pubKey *btcec.PublicKey) ([32]byte, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	key := [32]byte{}
	req := &signrpc.SharedKeyRequest{
		EphemeralPubkey: pubKey.SerializeCompressed(),
		KeyDesc: &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: int32(keyDesc.Family),
				KeyIndex:  int32(keyDesc.Index),
			},
		},
	}

	if keyDesc.Index == 0 && keyDesc.PubKey != nil {
		req.KeyDesc.RawKeyBytes = keyDesc.PubKey.SerializeCompressed()
	}

	resp, err := r.signerClient.DeriveSharedKey(ctxt, req)
	if err != nil {
		err = fmt.Errorf("error deriving shared key in remote signer "+
			"instance: %v", err)

		// Log as critical as we should shut down if there is no signer.
		log.Criticalf("RPC signer error: %v", err)
		return key, err
	}

	copy(key[:], resp.SharedKey)
	return key, nil
}

// SignMessage attempts to sign a target message with the private key described
// in the key locator. If the target private key is unable to be found, then an
// error will be returned. The actual digest signed is the single or double
// SHA-256 of the passed message.
//
// NOTE: This method is part of the keychain.MessageSignerRing interface.
func (r *RPCKeyRing) SignMessage(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) (*btcec.Signature, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.SignMessage(ctxt, &signrpc.SignMessageReq{
		Msg: msg,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyLoc.Family),
			KeyIndex:  int32(keyLoc.Index),
		},
		DoubleHash: doubleHash,
	})
	if err != nil {
		err = fmt.Errorf("error signing message in remote signer "+
			"instance: %v", err)

		// Log as critical as we should shut down if there is no signer.
		log.Criticalf("RPC signer error: %v", err)
		return nil, err
	}

	wireSig, err := lnwire.NewSigFromRawSignature(resp.Signature)
	if err != nil {
		return nil, fmt.Errorf("error parsing raw signature: %v", err)
	}
	return wireSig.ToSignature()
}

// SignMessageCompact signs the given message, single or double SHA256 hashing
// it first, with the private key described in the key locator and returns the
// signature in the compact, public key recoverable format.
//
// NOTE: This method is part of the keychain.MessageSignerRing interface.
func (r *RPCKeyRing) SignMessageCompact(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) ([]byte, error) {

	if keyLoc.Family != keychain.KeyFamilyNodeKey {
		return nil, fmt.Errorf("error compact signing with key "+
			"locator %v, can only sign with node key", keyLoc)
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.SignMessage(ctxt, &signrpc.SignMessageReq{
		Msg: msg,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyLoc.Family),
			KeyIndex:  int32(keyLoc.Index),
		},
		DoubleHash: doubleHash,
		CompactSig: true,
	})
	if err != nil {
		err = fmt.Errorf("error signing message in remote signer "+
			"instance: %v", err)

		// Log as critical as we should shut down if there is no signer.
		log.Criticalf("RPC signer error: %v", err)
		return nil, err
	}

	// The signature in the response is zbase32 encoded, so we need to
	// decode it before returning.
	return resp.Signature, nil
}

// DerivePrivKey attempts to derive the private key that corresponds to the
// passed key descriptor.  If the public key is set, then this method will
// perform an in-order scan over the key set, with a max of MaxKeyRangeScan
// keys. In order for this to work, the caller MUST set the KeyFamily within the
// partially populated KeyLocator.
//
// NOTE: This method is part of the keychain.SecretKeyRing interface.
func (r *RPCKeyRing) DerivePrivKey(_ keychain.KeyDescriptor) (*btcec.PrivateKey,
	error) {

	// This operation is not supported with remote signing. There should be
	// no need for invoking this method unless a channel backup (SCB) file
	// for pre-0.13.0 channels are attempted to be restored. In that case
	// it is recommended to restore the channels using a node with the full
	// seed available.
	return nil, ErrRemoteSigningPrivateKeyNotAvailable
}

// SignOutputRaw generates a signature for the passed transaction
// according to the data within the passed SignDescriptor.
//
// NOTE: The resulting signature should be void of a sighash byte.
//
// NOTE: This method is part of the input.Signer interface.
//
// NOTE: This method only signs with BIP1017 (internal) keys!
func (r *RPCKeyRing) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	// Forward the call to the remote signing instance. This call is only
	// ever called for signing witness (p2pkh or p2wsh) inputs and never
	// nested witness inputs, so the sigScript is always nil.
	return r.remoteSign(tx, signDesc, nil)
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed
// SignDescriptor. This method should be capable of generating the
// proper input script for both regular p2wkh output and p2wkh outputs
// nested within a regular p2sh output.
//
// NOTE: This method will ignore any tweak parameters set within the
// passed SignDescriptor as it assumes a set of typical script
// templates (p2wkh, np2wkh, etc).
//
// NOTE: This method is part of the input.Signer interface.
func (r *RPCKeyRing) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	addr, witnessProgram, sigScript, err := r.WalletController.ScriptForOutput(
		signDesc.Output,
	)
	if err != nil {
		return nil, err
	}
	signDesc.WitnessScript = witnessProgram

	// Let's give the TX to the remote instance now, so it can sign the
	// input.
	sig, err := r.remoteSign(tx, signDesc, sigScript)
	if err != nil {
		return nil, fmt.Errorf("error signing with remote instance: %v",
			err)
	}

	// ComputeInputScript currently is only used for P2WKH and NP2WKH
	// addresses. So the last item on the stack is always the compressed
	// public key.
	return &input.Script{
		Witness: wire.TxWitness{
			append(sig.Serialize(), byte(signDesc.HashType)),
			addr.PubKey().SerializeCompressed(),
		},
		SigScript: sigScript,
	}, nil
}

// remoteSign signs the input specified in signDesc of the given transaction tx
// using the remote signing instance.
func (r *RPCKeyRing) remoteSign(tx *wire.MsgTx, signDesc *input.SignDescriptor,
	sigScript []byte) (input.Signature, error) {

	packet, err := packetFromTx(tx)
	if err != nil {
		return nil, fmt.Errorf("error converting TX into PSBT: %v", err)
	}

	// Catch incorrect signing input index, just in case.
	if signDesc.InputIndex < 0 || signDesc.InputIndex >= len(packet.Inputs) {
		return nil, fmt.Errorf("invalid input index in sign descriptor")
	}
	in := &packet.Inputs[signDesc.InputIndex]
	txIn := tx.TxIn[signDesc.InputIndex]

	// Make sure we actually know about the input. We either have been
	// watching the UTXO on-chain or we have been given all the required
	// info in the sign descriptor.
	info, err := r.WalletController.FetchInputInfo(&txIn.PreviousOutPoint)
	switch {
	// No error, we do have the full UTXO and derivation info available.
	case err == nil:
		in.WitnessUtxo = &wire.TxOut{
			Value:    int64(info.Value),
			PkScript: info.PkScript,
		}
		in.NonWitnessUtxo = info.PrevTx
		in.Bip32Derivation = []*psbt.Bip32Derivation{info.Derivation}

	// The wallet doesn't know about this UTXO, so it's probably a TX that
	// we haven't published yet (e.g. a channel funding TX). So we need to
	// assemble everything from the sign descriptor. We won't be able to
	// supply a non-witness UTXO (=full TX of the input being spent) in this
	// case. That is no problem if the signing instance is another lnd
	// instance since we don't require it for pure witness inputs. But a
	// hardware wallet might require it for security reasons.
	case signDesc.KeyDesc.PubKey != nil && signDesc.Output != nil:
		in.WitnessUtxo = signDesc.Output
		in.Bip32Derivation = []*psbt.Bip32Derivation{{
			Bip32Path: []uint32{
				keychain.BIP0043Purpose +
					hdkeychain.HardenedKeyStart,
				r.coinType + hdkeychain.HardenedKeyStart,
				uint32(signDesc.KeyDesc.Family) +
					hdkeychain.HardenedKeyStart,
				0,
				signDesc.KeyDesc.Index,
			},
			PubKey: signDesc.KeyDesc.PubKey.SerializeCompressed(),
		}}

	default:
		return nil, fmt.Errorf("error assembling UTXO information, "+
			"wallet returned err='%v' and sign descriptor is "+
			"incomplete", err)
	}

	// Assemble all other information about the input we have.
	in.RedeemScript = sigScript
	in.SighashType = signDesc.HashType
	in.WitnessScript = signDesc.WitnessScript

	if len(signDesc.SingleTweak) > 0 {
		in.Unknowns = append(in.Unknowns, &psbt.Unknown{
			Key:   btcwallet.PsbtKeyTypeInputSignatureTweakSingle,
			Value: signDesc.SingleTweak,
		})
	}
	if signDesc.DoubleTweak != nil {
		in.Unknowns = append(in.Unknowns, &psbt.Unknown{
			Key:   btcwallet.PsbtKeyTypeInputSignatureTweakDouble,
			Value: signDesc.DoubleTweak.Serialize(),
		})
	}

	// Okay, let's sign the input by the remote signer now.
	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("error serializing PSBT: %v", err)
	}

	resp, err := r.walletClient.SignPsbt(
		ctxt, &walletrpc.SignPsbtRequest{FundedPsbt: buf.Bytes()},
	)
	if err != nil {
		err = fmt.Errorf("error signing PSBT in remote signer "+
			"instance: %v", err)

		// Log as critical as we should shut down if there is no signer.
		log.Criticalf("RPC signer error: %v", err)
		return nil, err
	}

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(resp.SignedPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing signed PSBT: %v", err)
	}

	// We expect a signature in the input now.
	if signDesc.InputIndex >= len(signedPacket.Inputs) {
		return nil, fmt.Errorf("remote signer returned invalid PSBT")
	}
	in = &signedPacket.Inputs[signDesc.InputIndex]
	if len(in.PartialSigs) != 1 {
		return nil, fmt.Errorf("remote signer returned invalid "+
			"partial signature, wanted 1, got %d",
			len(in.PartialSigs))
	}
	sigWithSigHash := in.PartialSigs[0]
	if sigWithSigHash == nil {
		return nil, fmt.Errorf("remote signer returned nil signature")
	}

	// The remote signer always adds the sighash type, so we need to account
	// for that.
	if len(sigWithSigHash.Signature) < btcec.MinSigLen+1 {
		return nil, fmt.Errorf("remote signer returned invalid "+
			"partial signature: signature too short with %d bytes",
			len(sigWithSigHash.Signature))
	}

	// Parse the signature, but chop off the last byte which is the sighash
	// type.
	sig := sigWithSigHash.Signature[0 : len(sigWithSigHash.Signature)-1]
	return btcec.ParseDERSignature(sig, btcec.S256())
}

// connectRPC tries to establish an RPC connection to the given host:port with
// the supplied certificate and macaroon.
func connectRPC(hostPort, tlsCertPath, macaroonPath string,
	timeout time.Duration) (*grpc.ClientConn, error) {

	certBytes, err := ioutil.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert file %v: %v",
			tlsCertPath, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}

	macBytes, err := ioutil.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("error reading macaroon file %v: %v",
			macaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("error decoding macaroon: %v", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error creating creds: %v", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(
			cp, "",
		)),
		grpc.WithPerRPCCredentials(macCred),
		grpc.WithBlock(),
	}
	ctxt, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := grpc.DialContext(ctxt, hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

// packetFromTx creates a PSBT from a tx that potentially already contains
// signed inputs.
func packetFromTx(original *wire.MsgTx) (*psbt.Packet, error) {
	// The psbt.NewFromUnsignedTx function complains if there are any
	// scripts or witness content on a TX. So we create a copy of the TX and
	// nil out all the offending data, but also keep a backup around that we
	// add to the PSBT afterwards.
	noSigs := original.Copy()
	for idx := range noSigs.TxIn {
		noSigs.TxIn[idx].SignatureScript = nil
		noSigs.TxIn[idx].Witness = nil
	}

	// With all the data that is seen as "signed", we can now create the
	// empty packet.
	packet, err := psbt.NewFromUnsignedTx(noSigs)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	for idx, txIn := range original.TxIn {
		if len(txIn.SignatureScript) > 0 {
			packet.Inputs[idx].FinalScriptSig = txIn.SignatureScript
		}

		if len(txIn.Witness) > 0 {
			buf.Reset()
			err = psbt.WriteTxWitness(&buf, txIn.Witness)
			if err != nil {
				return nil, err
			}
			packet.Inputs[idx].FinalScriptWitness = buf.Bytes()
		}
	}

	return packet, nil
}
