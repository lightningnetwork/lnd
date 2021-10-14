package rpcwallet

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
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
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const (
	// DefaultRPCTimeout is the default timeout that is used when forwarding
	// a request to the remote signer through RPC.
	DefaultRPCTimeout = 5 * time.Second
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
	rpcTimeout time.Duration) (*RPCKeyRing, error) {

	rpcConn, err := connectRPC(
		remoteSigner.RPCHost, remoteSigner.TLSCertPath,
		remoteSigner.MacaroonPath,
	)
	if err != nil {
		return nil, fmt.Errorf("error connecting to the remote "+
			"signing node through RPC: %v", err)
	}

	return &RPCKeyRing{
		WalletController: watchOnlyWalletController,
		watchOnlyKeyRing: watchOnlyKeyRing,
		rpcTimeout:       rpcTimeout,
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

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	rpcAddrType := walletrpc.AddressType_WITNESS_PUBKEY_HASH
	if addrType == lnwallet.NestedWitnessPubKey {
		rpcAddrType = walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH
	}

	remoteAddr, err := r.walletClient.NextAddr(ctxt, &walletrpc.AddrRequest{
		Account: account,
		Type:    rpcAddrType,
		Change:  change,
	})
	if err != nil {
		return nil, fmt.Errorf("error deriving address on remote "+
			"signer instance: %v", err)
	}

	localAddr, err := r.WalletController.NewAddress(
		addrType, change, account,
	)
	if err != nil {
		return nil, fmt.Errorf("error deriving address on local "+
			"wallet instance: %v", err)
	}

	// We need to make sure we've derived the same address on the remote
	// signing machine, otherwise we don't know whether we're at the same
	// address index (and therefore the same wallet state in general).
	if localAddr.String() != remoteAddr.Addr {
		return nil, fmt.Errorf("error deriving address on remote "+
			"signing instance, got different address (%s) than "+
			"on local wallet instance (%s)", remoteAddr.Addr,
			localAddr.String())
	}

	return localAddr, nil
}

// LastUnusedAddress returns the last *unused* address known by the wallet. An
// address is unused if it hasn't received any payments. This can be useful in
// UIs in order to continually show the "freshest" address without having to
// worry about "address inflation" caused by continual refreshing. Similar to
// NewAddress it can derive a specified address type, and also optionally a
// change address. The account parameter must be non-empty as it determines
// which account the address should be generated from.
func (r *RPCKeyRing) LastUnusedAddress(lnwallet.AddressType,
	string) (btcutil.Address, error) {

	// Because the underlying wallet will create a new address if the last
	// derived address has been used in the meantime, we would need to proxy
	// that call as well. But since that's deep within the btcwallet code,
	// we cannot easily proxy it without more refactoring. Since this is an
	// address type that is probably not widely used we can probably get
	// away with not supporting it.
	return nil, fmt.Errorf("unused address types are not supported when " +
		"remote signing is enabled")
}

// ImportAccount imports an account backed by an account extended public key.
// The master key fingerprint denotes the fingerprint of the root key
// corresponding to the account public key (also known as the key with
// derivation path m/). This may be required by some hardware wallets for proper
// identification and signing.
//
// The address type can usually be inferred from the key's version, but may be
// required for certain keys to map them into the proper scope.
//
// For BIP-0044 keys, an address type must be specified as we intend to not
// support importing BIP-0044 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will force
// the standard BIP-0049 derivation scheme, while a witness address type will
// force the standard BIP-0084 derivation scheme.
//
// For BIP-0049 keys, an address type must also be specified to make a
// distinction between the standard BIP-0049 address schema (nested witness
// pubkeys everywhere) and our own BIP-0049Plus address schema (nested pubkeys
// externally, witness pubkeys internally).
func (r *RPCKeyRing) ImportAccount(name string,
	accountPubKey *hdkeychain.ExtendedKey, masterKeyFingerprint uint32,
	addrType *waddrmgr.AddressType,
	dryRun bool) (*waddrmgr.AccountProperties, []btcutil.Address,
	[]btcutil.Address, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var masterKeyFingerprintBytes [4]byte
	binary.BigEndian.PutUint32(
		masterKeyFingerprintBytes[:], masterKeyFingerprint,
	)

	rpcAddrType, err := toRPCAddrType(addrType)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error converting address "+
			"type: %v", err)
	}

	remoteAcct, err := r.walletClient.ImportAccount(
		ctxt, &walletrpc.ImportAccountRequest{
			Name:                 name,
			ExtendedPublicKey:    accountPubKey.String(),
			MasterKeyFingerprint: masterKeyFingerprintBytes[:],
			AddressType:          rpcAddrType,
			DryRun:               dryRun,
		},
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error importing account on "+
			"remote signer instance: %v", err)
	}

	props, extAddrs, intAddrs, err := r.WalletController.ImportAccount(
		name, accountPubKey, masterKeyFingerprint, addrType, dryRun,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error importing account on "+
			"local wallet instance: %v", err)
	}

	mismatchErr := fmt.Errorf("error importing account on remote signing "+
		"instance, got different external addresses (%v) than on "+
		"local wallet instance (%s)", remoteAcct.DryRunExternalAddrs,
		extAddrs)
	if len(remoteAcct.DryRunExternalAddrs) != len(extAddrs) {
		return nil, nil, nil, mismatchErr
	}
	for idx, remoteExtAddr := range remoteAcct.DryRunExternalAddrs {
		if extAddrs[idx].String() != remoteExtAddr {
			return nil, nil, nil, mismatchErr
		}
	}

	mismatchErr = fmt.Errorf("error importing account on remote signing "+
		"instance, got different internal addresses (%v) than on "+
		"local wallet instance (%s)", remoteAcct.DryRunInternalAddrs,
		intAddrs)
	if len(remoteAcct.DryRunInternalAddrs) != len(intAddrs) {
		return nil, nil, nil, mismatchErr
	}
	for idx, remoteIntAddr := range remoteAcct.DryRunInternalAddrs {
		if intAddrs[idx].String() != remoteIntAddr {
			return nil, nil, nil, mismatchErr
		}
	}

	return props, extAddrs, intAddrs, nil
}

// ImportPublicKey imports a single derived public key into the wallet. The
// address type can usually be inferred from the key's version, but in the case
// of legacy versions (xpub, tpub), an address type must be specified as we
// intend to not support importing BIP-44 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme.
func (r *RPCKeyRing) ImportPublicKey(pubKey *btcec.PublicKey,
	addrType waddrmgr.AddressType) error {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	rpcAddrType, err := toRPCAddrType(&addrType)
	if err != nil {
		return fmt.Errorf("error converting address type: %v", err)
	}

	_, err = r.walletClient.ImportPublicKey(
		ctxt, &walletrpc.ImportPublicKeyRequest{
			PublicKey:   pubKey.SerializeCompressed(),
			AddressType: rpcAddrType,
		},
	)
	if err != nil {
		return fmt.Errorf("error importing pubkey on remote signer "+
			"instance: %v", err)
	}

	err = r.WalletController.ImportPublicKey(pubKey, addrType)
	if err != nil {
		return fmt.Errorf("error importing pubkey on local signer "+
			"instance: %v", err)
	}

	return nil
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (r *RPCKeyRing) SendOutputs(outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, minConfs int32,
	label string) (*wire.MsgTx, error) {

	tx, err := r.WalletController.SendOutputs(
		outputs, feeRate, minConfs, label,
	)
	if err != nil && err != btcwallet.ErrTxUnsigned {
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
		info, err := r.coinFromOutPoint(txIn.PreviousOutPoint)
		if err != nil {
			return nil, fmt.Errorf("error looking up utxo: %v", err)
		}

		// Now that we know the input is ours, we'll populate the
		// signDesc with the per input unique information.
		signDesc.Output = &wire.TxOut{
			Value:    info.Value,
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
// This is a part of the WalletController interface.
func (r *RPCKeyRing) FinalizePsbt(packet *psbt.Packet, accountName string) error {
	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return fmt.Errorf("error serializing PSBT: %v", err)
	}

	resp, err := r.walletClient.FinalizePsbt(
		ctxt, &walletrpc.FinalizePsbtRequest{
			FundedPsbt: buf.Bytes(),
			Account:    accountName,
		},
	)
	if err != nil {
		return fmt.Errorf("error finalizing PSBT in remote signer "+
			"instance: %v", err)
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

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This method is part of the keychain.KeyRing interface.
func (r *RPCKeyRing) DeriveNextKey(
	keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	// We need to keep the local and remote wallet in sync. That's why we
	// first attempt to also derive the next key on the remote wallet.
	remoteDesc, err := r.walletClient.DeriveNextKey(ctxt, &walletrpc.KeyReq{
		KeyFamily: int32(keyFam),
	})
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on remote signer instance: %v", err)
	}

	localDesc, err := r.watchOnlyKeyRing.DeriveNextKey(keyFam)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on local wallet instance: %v", err)
	}

	// We never know if the administrator of the remote signing wallet does
	// manual calls to next address or whatever. So we cannot be certain
	// that we're always fully in sync. But as long as our local index is
	// lower or equal to the remote index we know the remote wallet should
	// have all keys we have locally. Only if the remote wallet falls behind
	// the local we might have problems that the remote wallet won't know
	// outputs we're giving it to sign.
	if uint32(remoteDesc.KeyLoc.KeyIndex) < localDesc.Index {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on remote signer instance, derived index %d was "+
			"lower than local index %d", remoteDesc.KeyLoc.KeyIndex,
			localDesc.Index)
	}

	return localDesc, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This method is part of the keychain.KeyRing interface.
func (r *RPCKeyRing) DeriveKey(
	keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	// We need to keep the local and remote wallet in sync. That's why we
	// first attempt to also derive the same key on the remote wallet.
	remoteDesc, err := r.walletClient.DeriveKey(ctxt, &signrpc.KeyLocator{
		KeyFamily: int32(keyLoc.Family),
		KeyIndex:  int32(keyLoc.Index),
	})
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on remote signer instance: %v", err)
	}

	localDesc, err := r.watchOnlyKeyRing.DeriveKey(keyLoc)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on local wallet instance: %v", err)
	}

	// We never know if the administrator of the remote signing wallet does
	// manual calls to next address or whatever. So we cannot be certain
	// that we're always fully in sync. But as long as our local index is
	// lower or equal to the remote index we know the remote wallet should
	// have all keys we have locally. Only if the remote wallet falls behind
	// the local we might have problems that the remote wallet won't know
	// outputs we're giving it to sign.
	if uint32(remoteDesc.KeyLoc.KeyIndex) < localDesc.Index {
		return keychain.KeyDescriptor{}, fmt.Errorf("error deriving "+
			"key on remote signer instance, derived index %d was "+
			"lower than local index %d", remoteDesc.KeyLoc.KeyIndex,
			localDesc.Index)
	}

	return localDesc, nil
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
func (r *RPCKeyRing) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	rpcSignReq, err := toRPCSignReq(tx, signDesc)
	if err != nil {
		return nil, err
	}

	resp, err := r.signerClient.SignOutputRaw(ctxt, rpcSignReq)
	if err != nil {
		return nil, err
	}

	return btcec.ParseDERSignature(resp.RawSigs[0], btcec.S256())
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

	ctxt, cancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer cancel()

	rpcSignReq, err := toRPCSignReq(tx, signDesc)
	if err != nil {
		return nil, err
	}

	resp, err := r.signerClient.ComputeInputScript(ctxt, rpcSignReq)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness:   resp.InputScripts[0].Witness,
		SigScript: resp.InputScripts[0].SigScript,
	}, nil
}

// coinFromOutPoint attempts to locate details pertaining to a coin based on
// its outpoint. If the coin isn't under the control of the backing watch-only
// wallet, then an error is returned.
func (r *RPCKeyRing) coinFromOutPoint(op wire.OutPoint) (*chanfunding.Coin,
	error) {

	inputInfo, err := r.WalletController.FetchInputInfo(&op)
	if err != nil {
		return nil, err
	}

	return &chanfunding.Coin{
		TxOut: wire.TxOut{
			Value:    int64(inputInfo.Value),
			PkScript: inputInfo.PkScript,
		},
		OutPoint: inputInfo.OutPoint,
	}, nil
}

// toRPCSignReq converts the given raw transaction and sign descriptors into
// their corresponding RPC counterparts.
func toRPCSignReq(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*signrpc.SignReq, error) {

	if signDesc.Output == nil {
		return nil, fmt.Errorf("need output to sign")
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, err
	}

	rpcSignDesc := &signrpc.SignDescriptor{
		KeyDesc: &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: int32(signDesc.KeyDesc.Family),
				KeyIndex:  int32(signDesc.KeyDesc.Index),
			},
		},
		SingleTweak:   signDesc.SingleTweak,
		WitnessScript: signDesc.WitnessScript,
		Output: &signrpc.TxOut{
			Value:    signDesc.Output.Value,
			PkScript: signDesc.Output.PkScript,
		},
		Sighash:    uint32(signDesc.HashType),
		InputIndex: int32(signDesc.InputIndex),
	}

	if signDesc.KeyDesc.PubKey != nil {
		rpcSignDesc.KeyDesc.RawKeyBytes =
			signDesc.KeyDesc.PubKey.SerializeCompressed()
	}
	if signDesc.DoubleTweak != nil {
		rpcSignDesc.DoubleTweak = signDesc.DoubleTweak.Serialize()
	}

	return &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs:  []*signrpc.SignDescriptor{rpcSignDesc},
	}, nil
}

// toRPCAddrType converts the given address type to its RPC counterpart.
func toRPCAddrType(addrType *waddrmgr.AddressType) (walletrpc.AddressType,
	error) {

	if addrType == nil {
		return walletrpc.AddressType_UNKNOWN, nil
	}

	switch *addrType {
	case waddrmgr.WitnessPubKey:
		return walletrpc.AddressType_WITNESS_PUBKEY_HASH, nil

	case waddrmgr.NestedWitnessPubKey:
		return walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH,
			nil

	default:
		return 0, fmt.Errorf("unhandled address type %v", *addrType)
	}
}

// connectRPC tries to establish an RPC connection to the given host:port with
// the supplied certificate and macaroon.
func connectRPC(hostPort, tlsCertPath, macaroonPath string) (*grpc.ClientConn,
	error) {

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
	}
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}
