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
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
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
	watchOnlyWallet keychain.SecretKeyRing

	rpcTimeout time.Duration

	signerClient signrpc.SignerClient
	walletClient walletrpc.WalletKitClient
}

var _ keychain.SecretKeyRing = (*RPCKeyRing)(nil)
var _ input.Signer = (*RPCKeyRing)(nil)
var _ keychain.MessageSignerRing = (*RPCKeyRing)(nil)

// NewRPCKeyRing creates a new remote signing secret key ring that uses the
// given watch-only base wallet to keep track of addresses and transactions but
// delegates any signing or ECDH operations to the remove signer through RPC.
func NewRPCKeyRing(watchOnlyWallet keychain.SecretKeyRing,
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
		watchOnlyWallet: watchOnlyWallet,
		rpcTimeout:      rpcTimeout,
		signerClient:    signrpc.NewSignerClient(rpcConn),
		walletClient:    walletrpc.NewWalletKitClient(rpcConn),
	}, nil
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

	localDesc, err := r.watchOnlyWallet.DeriveNextKey(keyFam)
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

	localDesc, err := r.watchOnlyWallet.DeriveKey(keyLoc)
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
