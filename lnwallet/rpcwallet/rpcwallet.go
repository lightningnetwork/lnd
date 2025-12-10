package rpcwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/fn/v2"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
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

	netParams *chaincfg.Params

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
	remoteSigner *lncfg.RemoteSigner,
	netParams *chaincfg.Params) (*RPCKeyRing, error) {

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
		netParams:        netParams,
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
func (r *RPCKeyRing) SendOutputs(inputs fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight,
	minConfs int32, label string,
	strategy basewallet.CoinSelectionStrategy) (*wire.MsgTx, error) {

	tx, err := r.WalletController.SendOutputs(
		inputs, outputs, feeRate, minConfs, label, strategy,
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
	outputFetcher := lnwallet.NewWalletPrevOutputFetcher(r.WalletController)
	for i, txIn := range tx.TxIn {
		signDesc := input.SignDescriptor{
			HashType: txscript.SigHashAll,
			SigHashes: txscript.NewTxSigHashes(
				tx, outputFetcher,
			),
			PrevOutputFetcher: outputFetcher,
		}

		// We can only sign this input if it's ours, so we'll ask the
		// watch-only wallet if it can map this outpoint into a coin we
		// own. If not, then we can't continue because our wallet state
		// is out of sync.
		info, err := r.WalletController.FetchOutpointInfo(
			&txIn.PreviousOutPoint,
		)
		if err != nil {
			return nil, fmt.Errorf("error looking up utxo: %w", err)
		}

		if txscript.IsPayToTaproot(info.PkScript) {
			signDesc.HashType = txscript.SigHashDefault
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
func (r *RPCKeyRing) SignPsbt(packet *psbt.Packet) ([]uint32, error) {
	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("error serializing PSBT: %w", err)
	}

	resp, err := r.walletClient.SignPsbt(ctxt, &walletrpc.SignPsbtRequest{
		FundedPsbt: buf.Bytes(),
	})
	if err != nil {
		considerShutdown(err)
		return nil, fmt.Errorf("error signing PSBT in remote signer "+
			"instance: %v", err)
	}

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(resp.SignedPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing signed PSBT: %w", err)
	}

	// The caller expects the packet to be modified instead of a new
	// instance to be returned. So we just overwrite all fields in the
	// original packet.
	packet.UnsignedTx = signedPacket.UnsignedTx
	packet.Inputs = signedPacket.Inputs
	packet.Outputs = signedPacket.Outputs
	packet.Unknowns = signedPacket.Unknowns

	return resp.SignedInputs, nil
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
	// We need at least one input and one output. In addition each
	// input needs nonWitness Utxo or witness Utxo data specified.
	err := psbt.InputsReadyToSign(packet)
	if err != nil {
		return err
	}

	// Go through each input that doesn't have final witness data attached
	// to it already and try to sign it. We do expect that we're the last
	// ones to sign. If there is any input without witness data that we
	// cannot sign because it's not our UTXO, this will be a hard failure.
	tx := packet.UnsignedTx
	prevOutFetcher := basewallet.PsbtPrevOutputFetcher(packet)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
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
		utxo, err := r.FetchOutpointInfo(&txIn.PreviousOutPoint)
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
			HashType:          in.SighashType,
			SigHashes:         sigHashes,
			InputIndex:        idx,
			PrevOutputFetcher: prevOutFetcher,
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
			return fmt.Errorf("error serializing witness: %w", err)
		}
		packet.Inputs[idx].FinalScriptWitness = witnessBytes.Bytes()
		packet.Inputs[idx].FinalScriptSig = script.SigScript
	}

	// Make sure the PSBT itself thinks it's finalized and ready to be
	// broadcast.
	err = psbt.MaybeFinalizeAll(packet)
	if err != nil {
		return fmt.Errorf("error finalizing PSBT: %w", err)
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
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
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
		considerShutdown(err)
		return key, fmt.Errorf("error deriving shared key in remote "+
			"signer instance: %v", err)
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
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

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
		considerShutdown(err)
		return nil, fmt.Errorf("error signing message in remote "+
			"signer instance: %v", err)
	}

	wireSig, err := lnwire.NewSigFromECDSARawSignature(resp.Signature)
	if err != nil {
		return nil, fmt.Errorf("unable to create sig: %w", err)
	}
	sig, err := wireSig.ToSignature()
	if err != nil {
		return nil, fmt.Errorf("unable to parse sig: %w", err)
	}
	ecdsaSig, ok := sig.(*ecdsa.Signature)
	if !ok {
		return nil, fmt.Errorf("unexpected signature type: %T", sig)
	}

	return ecdsaSig, nil
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
		considerShutdown(err)
		return nil, fmt.Errorf("error signing message in remote "+
			"signer instance: %v", err)
	}

	// The signature in the response is zbase32 encoded, so we need to
	// decode it before returning.
	return resp.Signature, nil
}

// SignMessageSchnorr attempts to sign a target message with the private key
// described in the key locator. If the target private key is unable to be
// found, then an error will be returned. The actual digest signed is the
// single or double SHA-256 of the passed message.
//
// NOTE: This method is part of the keychain.MessageSignerRing interface.
func (r *RPCKeyRing) SignMessageSchnorr(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool, taprootTweak []byte,
	tag []byte) (*schnorr.Signature, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.SignMessage(ctxt, &signrpc.SignMessageReq{
		Msg: msg,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyLoc.Family),
			KeyIndex:  int32(keyLoc.Index),
		},
		DoubleHash:         doubleHash,
		SchnorrSig:         true,
		SchnorrSigTapTweak: taprootTweak,
		Tag:                tag,
	})
	if err != nil {
		considerShutdown(err)
		return nil, fmt.Errorf("error signing message in remote "+
			"signer instance: %w", err)
	}

	sigParsed, err := schnorr.ParseSignature(resp.Signature)
	if err != nil {
		return nil, fmt.Errorf("can't parse schnorr signature: %w",
			err)
	}
	return sigParsed, nil
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
// templates (p2wkh, np2wkh, BIP0086 p2tr, etc).
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

	// If this is a p2tr address, then it must be a BIP0086 key spend if we
	// are coming through this path (instead of SignOutputRaw).
	switch addr.AddrType() {
	case waddrmgr.TaprootPubKey:
		signDesc.SignMethod = input.TaprootKeySpendBIP0086SignMethod
		signDesc.WitnessScript = nil

		sig, err := r.remoteSign(tx, signDesc, nil)
		if err != nil {
			return nil, fmt.Errorf("error signing with remote"+
				"instance: %v", err)
		}

		rawSig := sig.Serialize()
		if signDesc.HashType != txscript.SigHashDefault {
			rawSig = append(rawSig, byte(signDesc.HashType))
		}

		return &input.Script{
			Witness: wire.TxWitness{
				rawSig,
			},
		}, nil

	case waddrmgr.TaprootScript:
		return nil, fmt.Errorf("computing input script for taproot " +
			"script address not supported")
	}

	// Let's give the TX to the remote instance now, so it can sign the
	// input.
	sig, err := r.remoteSign(tx, signDesc, witnessProgram)
	if err != nil {
		return nil, fmt.Errorf("error signing with remote instance: %w",
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

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (r *RPCKeyRing) MuSig2CreateSession(bipVersion input.MuSig2Version,
	keyLoc keychain.KeyLocator, pubKeys []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	apiVersion, err := signrpc.MarshalMuSig2Version(bipVersion)
	if err != nil {
		return nil, err
	}

	// We need to serialize all data for the RPC call. We can do that by
	// putting everything directly into the request struct.
	req := &signrpc.MuSig2SessionRequest{
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyLoc.Family),
			KeyIndex:  int32(keyLoc.Index),
		},
		AllSignerPubkeys: make([][]byte, len(pubKeys)),
		Tweaks: make(
			[]*signrpc.TweakDesc, len(tweaks.GenericTweaks),
		),
		OtherSignerPublicNonces: make([][]byte, len(otherNonces)),
		Version:                 apiVersion,
	}
	for idx, pubKey := range pubKeys {
		switch bipVersion {
		case input.MuSig2Version040:
			req.AllSignerPubkeys[idx] = schnorr.SerializePubKey(
				pubKey,
			)

		case input.MuSig2Version100RC2:
			req.AllSignerPubkeys[idx] = pubKey.SerializeCompressed()
		}
	}
	for idx, genericTweak := range tweaks.GenericTweaks {
		req.Tweaks[idx] = &signrpc.TweakDesc{
			Tweak:   genericTweak.Tweak[:],
			IsXOnly: genericTweak.IsXOnly,
		}
	}
	for idx, nonce := range otherNonces {
		req.OtherSignerPublicNonces[idx] = make([]byte, len(nonce))
		copy(req.OtherSignerPublicNonces[idx], nonce[:])
	}
	if tweaks.HasTaprootTweak() {
		req.TaprootTweak = &signrpc.TaprootTweakDesc{
			KeySpendOnly: tweaks.TaprootBIP0086Tweak,
			ScriptRoot:   tweaks.TaprootTweak,
		}
	}

	if localNonces != nil {
		req.PregeneratedLocalNonce = localNonces.SecNonce[:]
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.MuSig2CreateSession(ctxt, req)
	if err != nil {
		considerShutdown(err)
		return nil, fmt.Errorf("error creating MuSig2 session in "+
			"remote signer instance: %v", err)
	}

	// De-Serialize all the info back into our native struct.
	info := &input.MuSig2SessionInfo{
		Version:       bipVersion,
		TaprootTweak:  tweaks.HasTaprootTweak(),
		HaveAllNonces: resp.HaveAllNonces,
	}
	copy(info.SessionID[:], resp.SessionId)
	copy(info.PublicNonce[:], resp.LocalPublicNonces)

	info.CombinedKey, err = schnorr.ParsePubKey(resp.CombinedKey)
	if err != nil {
		return nil, fmt.Errorf("error parsing combined key: %w", err)
	}

	if tweaks.HasTaprootTweak() {
		info.TaprootInternalKey, err = schnorr.ParsePubKey(
			resp.TaprootInternalKey,
		)
		if err != nil {
			return nil, fmt.Errorf("error parsing internal key: %w",
				err)
		}
	}

	return info, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (r *RPCKeyRing) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	pubNonces [][musig2.PubNonceSize]byte) (bool, error) {

	// We need to serialize all data for the RPC call. We can do that by
	// putting everything directly into the request struct.
	req := &signrpc.MuSig2RegisterNoncesRequest{
		SessionId:               sessionID[:],
		OtherSignerPublicNonces: make([][]byte, len(pubNonces)),
	}
	for idx, nonce := range pubNonces {
		req.OtherSignerPublicNonces[idx] = make([]byte, len(nonce))
		copy(req.OtherSignerPublicNonces[idx], nonce[:])
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.MuSig2RegisterNonces(ctxt, req)
	if err != nil {
		considerShutdown(err)
		return false, fmt.Errorf("error registering MuSig2 nonces in "+
			"remote signer instance: %v", err)
	}

	return resp.HaveAllNonces, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for a
// session identified by its ID. This is an alternative to MuSig2RegisterNonces
// and is used when a coordinator has already aggregated all individual nonces.
func (r *RPCKeyRing) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte) error {

	req := &signrpc.MuSig2RegisterCombinedNonceRequest{
		SessionId:           sessionID[:],
		CombinedPublicNonce: combinedNonce[:],
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	_, err := r.signerClient.MuSig2RegisterCombinedNonce(ctxt, req)
	if err != nil {
		considerShutdown(err)

		return fmt.Errorf("error registering MuSig2 combined nonce "+
			"in remote signer instance: %v", err)
	}

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session identified
// by its ID.
func (r *RPCKeyRing) MuSig2GetCombinedNonce(sessionID input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	req := &signrpc.MuSig2GetCombinedNonceRequest{
		SessionId: sessionID[:],
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.MuSig2GetCombinedNonce(ctxt, req)
	if err != nil {
		considerShutdown(err)

		return [musig2.PubNonceSize]byte{}, fmt.Errorf("error getting "+
			"MuSig2 combined nonce from remote signer instance: %v",
			err)
	}

	var combinedNonce [musig2.PubNonceSize]byte
	copy(combinedNonce[:], resp.CombinedPublicNonce)

	return combinedNonce, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (r *RPCKeyRing) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [sha256.Size]byte, cleanUp bool) (*musig2.PartialSignature, error) {

	// We need to serialize all data for the RPC call. We can do that by
	// putting everything directly into the request struct.
	req := &signrpc.MuSig2SignRequest{
		SessionId:     sessionID[:],
		MessageDigest: msg[:],
		Cleanup:       cleanUp,
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.MuSig2Sign(ctxt, req)
	if err != nil {
		considerShutdown(err)
		return nil, fmt.Errorf("error signing MuSig2 session in "+
			"remote signer instance: %v", err)
	}

	partialSig, err := input.DeserializePartialSignature(
		resp.LocalPartialSignature,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing partial signature from "+
			"remote signer: %v", err)
	}

	return partialSig, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (r *RPCKeyRing) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	// We need to serialize all data for the RPC call. We can do that by
	// putting everything directly into the request struct.
	req := &signrpc.MuSig2CombineSigRequest{
		SessionId:              sessionID[:],
		OtherPartialSignatures: make([][]byte, len(partialSigs)),
	}
	for idx, partialSig := range partialSigs {
		rawSig, err := input.SerializePartialSignature(partialSig)
		if err != nil {
			return nil, false, fmt.Errorf("error serializing "+
				"partial signature: %v", err)
		}
		req.OtherPartialSignatures[idx] = rawSig[:]
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	resp, err := r.signerClient.MuSig2CombineSig(ctxt, req)
	if err != nil {
		considerShutdown(err)
		return nil, false, fmt.Errorf("error combining MuSig2 "+
			"signatures in remote signer instance: %v", err)
	}

	// The final signature is only available when we have all the other
	// partial signatures from all participants.
	if !resp.HaveAllSignatures {
		return nil, resp.HaveAllSignatures, nil
	}

	finalSig, err := schnorr.ParseSignature(resp.FinalSignature)
	if err != nil {
		return nil, false, fmt.Errorf("error parsing final signature: "+
			"%v", err)
	}

	return finalSig, resp.HaveAllSignatures, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (r *RPCKeyRing) MuSig2Cleanup(sessionID input.MuSig2SessionID) error {
	req := &signrpc.MuSig2CleanupRequest{
		SessionId: sessionID[:],
	}

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	_, err := r.signerClient.MuSig2Cleanup(ctxt, req)
	if err != nil {
		considerShutdown(err)
		return fmt.Errorf("error cleaning up MuSig2 session in remote "+
			"signer instance: %v", err)
	}

	return nil
}

// remoteSign signs the input specified in signDesc of the given transaction tx
// using the remote signing instance.
func (r *RPCKeyRing) remoteSign(tx *wire.MsgTx, signDesc *input.SignDescriptor,
	sigScript []byte) (input.Signature, error) {

	packet, err := packetFromTx(tx)
	if err != nil {
		return nil, fmt.Errorf("error converting TX into PSBT: %w", err)
	}

	// We need to add witness information for all inputs! Otherwise, we'll
	// have a problem when attempting to sign a taproot input!
	for idx := range packet.Inputs {
		// Skip the input we're signing for, that will get a special
		// treatment later on.
		if idx == signDesc.InputIndex {
			continue
		}

		txIn := tx.TxIn[idx]
		info, err := r.WalletController.FetchOutpointInfo(
			&txIn.PreviousOutPoint,
		)
		if err != nil {
			// Maybe we have an UTXO in the previous output fetcher?
			if signDesc.PrevOutputFetcher != nil {
				utxo := signDesc.PrevOutputFetcher.FetchPrevOutput(
					txIn.PreviousOutPoint,
				)
				if utxo != nil && utxo.Value != 0 &&
					len(utxo.PkScript) > 0 {

					packet.Inputs[idx].WitnessUtxo = utxo
					continue
				}
			}

			log.Warnf("No UTXO info found for index %d "+
				"(prev_outpoint=%v), won't be able to sign "+
				"for taproot output!", idx,
				txIn.PreviousOutPoint)
			continue
		}
		packet.Inputs[idx].WitnessUtxo = &wire.TxOut{
			Value:    int64(info.Value),
			PkScript: info.PkScript,
		}
	}

	// Catch incorrect signing input index, just in case.
	if signDesc.InputIndex < 0 || signDesc.InputIndex >= len(packet.Inputs) {
		return nil, fmt.Errorf("invalid input index in sign descriptor")
	}
	in := &packet.Inputs[signDesc.InputIndex]
	txIn := tx.TxIn[signDesc.InputIndex]

	// Things are a bit tricky with the sign descriptor. There basically are
	// four ways to describe a key:
	//   1. By public key only. To match this case both family and index
	//      must be set to 0.
	//   2. By family and index only. To match this case the public key
	//      must be nil and either the family or index must be non-zero.
	//   3. All values are set and locator is non-empty. To match this case
	//      the public key must be set and either the family or index must
	//      be non-zero.
	//   4. All values are set and locator is empty. This is a special case
	//      for the very first channel ever created (with the multi-sig key
	//      family which is 0 and the index which is 0 as well). This looks
	//      identical to case 1 and will also be handled like that case.
	// We only really handle case 1 and 2 here, since 3 is no problem and 4
	// is identical to 1.
	switch {
	// Case 1: Public key only. We need to find out the derivation path for
	// this public key by asking the wallet. This is only possible for our
	// internal, custom 1017 scope since we know all keys derived there are
	// internally stored as p2wkh addresses.
	case signDesc.KeyDesc.PubKey != nil && signDesc.KeyDesc.IsEmpty():
		pubKeyBytes := signDesc.KeyDesc.PubKey.SerializeCompressed()
		addr, err := btcutil.NewAddressWitnessPubKeyHash(
			btcutil.Hash160(pubKeyBytes), r.netParams,
		)
		if err != nil {
			return nil, fmt.Errorf("error deriving address from "+
				"public key %x: %v", pubKeyBytes, err)
		}

		managedAddr, err := r.AddressInfo(addr)
		if err != nil {
			return nil, fmt.Errorf("error fetching address info "+
				"for public key %x: %v", pubKeyBytes, err)
		}

		pubKeyAddr, ok := managedAddr.(waddrmgr.ManagedPubKeyAddress)
		if !ok {
			return nil, fmt.Errorf("address derived for public "+
				"key %x is not a p2wkh address", pubKeyBytes)
		}

		scope, path, _ := pubKeyAddr.DerivationInfo()
		if scope.Purpose != keychain.BIP0043Purpose {
			return nil, fmt.Errorf("address derived for public "+
				"key %x is not in custom key scope %d'",
				pubKeyBytes, keychain.BIP0043Purpose)
		}

		// We now have all the information we need to complete our key
		// locator information.
		signDesc.KeyDesc.KeyLocator = keychain.KeyLocator{
			Family: keychain.KeyFamily(path.InternalAccount),
			Index:  path.Index,
		}

	// Case 2: Family and index only. This case is easy, we can just go
	// ahead and derive the public key from the family and index and then
	// supply that information in the BIP32 derivation field.
	case signDesc.KeyDesc.PubKey == nil && !signDesc.KeyDesc.IsEmpty():
		fullDesc, err := r.watchOnlyKeyRing.DeriveKey(
			signDesc.KeyDesc.KeyLocator,
		)
		if err != nil {
			return nil, fmt.Errorf("error deriving key with "+
				"family %d and index %d from the watch-only "+
				"wallet: %v",
				signDesc.KeyDesc.KeyLocator.Family,
				signDesc.KeyDesc.KeyLocator.Index, err)
		}
		signDesc.KeyDesc.PubKey = fullDesc.PubKey
	}

	var derivation *psbt.Bip32Derivation

	// Make sure we actually know about the input. We either have been
	// watching the UTXO on-chain or we have been given all the required
	// info in the sign descriptor.
	info, err := r.WalletController.FetchOutpointInfo(
		&txIn.PreviousOutPoint,
	)

	// If the wallet is aware of this outpoint, we go ahead and fetch the
	// derivation info.
	if err == nil {
		derivation, err = r.WalletController.FetchDerivationInfo(
			info.PkScript,
		)
	}

	switch {
	// No error, we do have the full UTXO info available.
	case err == nil:
		in.WitnessUtxo = &wire.TxOut{
			Value:    int64(info.Value),
			PkScript: info.PkScript,
		}
		in.NonWitnessUtxo = info.PrevTx
		in.Bip32Derivation = []*psbt.Bip32Derivation{derivation}

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
				r.netParams.HDCoinType +
					hdkeychain.HardenedKeyStart,
				uint32(signDesc.KeyDesc.Family) +
					hdkeychain.HardenedKeyStart,
				0,
				signDesc.KeyDesc.Index,
			},
			PubKey: signDesc.KeyDesc.PubKey.SerializeCompressed(),
		}}

		// We need to specify a pk script in the witness UTXO, otherwise
		// the field becomes invalid when serialized as a PSBT. To avoid
		// running into a generic "Invalid PSBT serialization format"
		// error later, we return a more descriptive error now.
		if len(in.WitnessUtxo.PkScript) == 0 {
			return nil, fmt.Errorf("error assembling UTXO " +
				"information, output not known to wallet and " +
				"no UTXO pk script provided in sign descriptor")
		}

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

	// Add taproot specific fields.
	switch signDesc.SignMethod {
	case input.TaprootKeySpendBIP0086SignMethod,
		input.TaprootKeySpendSignMethod:

		// The key identifying factor for a key spend is that we don't
		// provide any leaf hashes to signal we want a signature for the
		// key spend path (with the internal key).
		d := in.Bip32Derivation[0]
		in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
			// The x-only public key is just our compressed public
			// key without the first byte (type/parity).
			XOnlyPubKey:          d.PubKey[1:],
			LeafHashes:           nil,
			MasterKeyFingerprint: d.MasterKeyFingerprint,
			Bip32Path:            d.Bip32Path,
		}}

		// If this is a BIP0086 key spend then the tap tweak is empty,
		// otherwise it's set to the Taproot root hash.
		in.TaprootMerkleRoot = signDesc.TapTweak

	case input.TaprootScriptSpendSignMethod:
		// The script spend path is a bit more involved when doing it
		// through the PSBT method. We need to specify the leaf hash
		// that the signer should sign for.
		leaf := txscript.TapLeaf{
			LeafVersion: txscript.BaseLeafVersion,
			Script:      signDesc.WitnessScript,
		}
		leafHash := leaf.TapHash()

		d := in.Bip32Derivation[0]
		in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
			XOnlyPubKey:          d.PubKey[1:],
			LeafHashes:           [][]byte{leafHash[:]},
			MasterKeyFingerprint: d.MasterKeyFingerprint,
			Bip32Path:            d.Bip32Path,
		}}

		// We also need to supply a control block. But because we don't
		// know the internal key nor the merkle proofs (both is not
		// supplied through the SignOutputRaw RPC) and is technically
		// not really needed by the signer (since we only want a
		// signature, the full witness stack is assembled by the caller
		// of this RPC), we can get by with faking certain information
		// that we don't have.
		fakeInternalKey, _ := btcec.ParsePubKey(d.PubKey)
		fakeKeyIsOdd := d.PubKey[0] == input.PubKeyFormatCompressedOdd
		controlBlock := txscript.ControlBlock{
			InternalKey:     fakeInternalKey,
			OutputKeyYIsOdd: fakeKeyIsOdd,
			LeafVersion:     leaf.LeafVersion,
		}
		blockBytes, err := controlBlock.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("error serializing control "+
				"block: %v", err)
		}

		in.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
			ControlBlock: blockBytes,
			Script:       leaf.Script,
			LeafVersion:  leaf.LeafVersion,
		}}
	}

	// Okay, let's sign the input by the remote signer now.
	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("error serializing PSBT: %w", err)
	}

	resp, err := r.walletClient.SignPsbt(
		ctxt, &walletrpc.SignPsbtRequest{FundedPsbt: buf.Bytes()},
	)
	if err != nil {
		considerShutdown(err)
		return nil, fmt.Errorf("error signing PSBT in remote signer "+
			"instance: %v", err)
	}

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(resp.SignedPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing signed PSBT: %w", err)
	}

	// We expect a signature in the input now.
	if signDesc.InputIndex >= len(signedPacket.Inputs) {
		return nil, fmt.Errorf("remote signer returned invalid PSBT")
	}
	in = &signedPacket.Inputs[signDesc.InputIndex]

	return extractSignature(in, signDesc.SignMethod)
}

// extractSignature attempts to extract the signature from the PSBT input,
// looking at different fields depending on the signing method that was used.
func extractSignature(in *psbt.PInput,
	signMethod input.SignMethod) (input.Signature, error) {

	switch signMethod {
	case input.WitnessV0SignMethod:
		if len(in.PartialSigs) != 1 {
			return nil, fmt.Errorf("remote signer returned "+
				"invalid partial signature, wanted 1, got %d",
				len(in.PartialSigs))
		}
		sigWithSigHash := in.PartialSigs[0]
		if sigWithSigHash == nil {
			return nil, fmt.Errorf("remote signer returned nil " +
				"signature")
		}

		// The remote signer always adds the sighash type, so we need to
		// account for that.
		sigLen := len(sigWithSigHash.Signature)
		if sigLen < ecdsa.MinSigLen+1 {
			return nil, fmt.Errorf("remote signer returned "+
				"invalid partial signature: signature too "+
				"short with %d bytes", sigLen)
		}

		// Parse the signature, but chop off the last byte which is the
		// sighash type.
		sig := sigWithSigHash.Signature[0 : sigLen-1]
		return ecdsa.ParseDERSignature(sig)

	// The type of key spend doesn't matter, the signature should be in the
	// same field for both of those signing methods.
	case input.TaprootKeySpendBIP0086SignMethod,
		input.TaprootKeySpendSignMethod:

		sigLen := len(in.TaprootKeySpendSig)
		if sigLen < schnorr.SignatureSize {
			return nil, fmt.Errorf("remote signer returned "+
				"invalid key spend signature: signature too "+
				"short with %d bytes", sigLen)
		}

		return schnorr.ParseSignature(
			in.TaprootKeySpendSig[:schnorr.SignatureSize],
		)

	case input.TaprootScriptSpendSignMethod:
		if len(in.TaprootScriptSpendSig) != 1 {
			return nil, fmt.Errorf("remote signer returned "+
				"invalid taproot script spend signature, "+
				"wanted 1, got %d",
				len(in.TaprootScriptSpendSig))
		}
		scriptSpendSig := in.TaprootScriptSpendSig[0]
		if scriptSpendSig == nil {
			return nil, fmt.Errorf("remote signer returned nil " +
				"taproot script spend signature")
		}

		return schnorr.ParseSignature(scriptSpendSig.Signature)

	default:
		return nil, fmt.Errorf("can't extract signature, unsupported "+
			"signing method: %v", signMethod)
	}
}

// connectRPC tries to establish an RPC connection to the given host:port with
// the supplied certificate and macaroon.
func connectRPC(hostPort, tlsCertPath, macaroonPath string,
	timeout time.Duration) (*grpc.ClientConn, error) {

	certBytes, err := os.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert file %v: %w",
			tlsCertPath, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}

	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("error reading macaroon file %v: %w",
			macaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("error decoding macaroon: %w", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error creating creds: %w", err)
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
		return nil, fmt.Errorf("unable to connect to RPC server: %w",
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

// considerShutdown inspects the error and issues a shutdown (through logging
// a critical error, which will cause the logger to issue a clean shutdown
// request) if the error looks like a connection or general availability error
// and not some application specific problem.
func considerShutdown(err error) {
	statusErr, isStatusErr := status.FromError(err)
	switch {
	// The context attached to the client request has timed out. This can be
	// due to not being able to reach the signing server, or it's taking too
	// long to respond. In either case, request a shutdown.
	case err == context.DeadlineExceeded:
		fallthrough

	// The signing server's context timed out before the client's due to
	// clock skew, request a shutdown anyway.
	case isStatusErr && statusErr.Code() == codes.DeadlineExceeded:
		log.Critical("RPC signing timed out: %v", err)

	case isStatusErr && statusErr.Code() == codes.Unavailable:
		log.Critical("RPC signing server not available: %v", err)
	}
}
