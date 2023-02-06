//go:build signrpc
// +build signrpc

package signrpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize this as the name of the
	// config file that we need.
	subServerName = "SignRPC"
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "signer",
			Action: "generate",
		},
		{
			Entity: "signer",
			Action: "read",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/signrpc.Signer/SignOutputRaw": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/ComputeInputScript": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/SignMessage": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/VerifyMessage": {{
			Entity: "signer",
			Action: "read",
		}},
		"/signrpc.Signer/DeriveSharedKey": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/MuSig2CombineKeys": {{
			Entity: "signer",
			Action: "read",
		}},
		"/signrpc.Signer/MuSig2CreateSession": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/MuSig2RegisterNonces": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/MuSig2Sign": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/MuSig2CombineSig": {{
			Entity: "signer",
			Action: "generate",
		}},
		"/signrpc.Signer/MuSig2Cleanup": {{
			Entity: "signer",
			Action: "generate",
		}},
	}

	// DefaultSignerMacFilename is the default name of the signer macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultSignerMacFilename = "signer.macaroon"
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	SignerServer
}

// Server is a sub-server of the main RPC server: the signer RPC. This sub RPC
// server allows external callers to access the full signing capabilities of
// lnd. This allows callers to create custom protocols, external to lnd, even
// backed by multiple distinct lnd across independent failure domains.
type Server struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	UnimplementedSignerServer

	cfg *Config
}

// A compile time check to ensure that Server fully implements the SignerServer
// gRPC service.
var _ SignerServer = (*Server)(nil)

// New returns a new instance of the signrpc Signer sub-server. We also return
// the set of permissions for the macaroons that we may create within this
// method. If the macaroons we need aren't found in the filepath, then we'll
// create them on start up. If we're unable to locate, or create the macaroons
// we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the signer macaroon wasn't generated, then we'll
	// assume that it's found at the default network directory.
	if cfg.SignerMacPath == "" {
		cfg.SignerMacPath = filepath.Join(
			cfg.NetworkDir, DefaultSignerMacFilename,
		)
	}

	// Now that we know the full path of the signer macaroon, we can check
	// to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	macFilePath := cfg.SignerMacPath
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Making macaroons for Signer RPC Server at: %v",
			macFilePath)

		// At this point, we know that the signer macaroon doesn't yet,
		// exist, so we need to create it with the help of the main
		// macaroon service.
		signerMac, err := cfg.MacService.NewMacaroon(
			context.Background(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		signerMacBytes, err := signerMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = ioutil.WriteFile(macFilePath, signerMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	signerServer := &Server{
		cfg: cfg,
	}

	return signerServer, macPermissions, nil
}

// Start launches any helper goroutines required for the rpcServer to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have
// requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterSignerServer(grpcServer, r)

	log.Debugf("Signer RPC server successfully register with root gRPC " +
		"server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterSignerHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Signer REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Signer REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.SignerServer = subServer
	return subServer, macPermissions, nil
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignReq. If we're unable to find the keys that
// correspond to the KeyLocators in the SignReq then we'll return an error.
// Additionally, if the user doesn't provide the set of required parameters, or
// provides an invalid transaction, then we'll return with an error.
//
// NOTE: The resulting signature should be void of a sighash byte.
func (s *Server) SignOutputRaw(_ context.Context, in *SignReq) (*SignResp,
	error) {

	switch {
	// If the client doesn't specify a transaction, then there's nothing to
	// sign, so we'll exit early.
	case len(in.RawTxBytes) == 0:
		return nil, fmt.Errorf("a transaction to sign MUST be " +
			"passed in")

	// If the client doesn't tell us *how* to sign the transaction, then we
	// can't sign anything, so we'll exit early.
	case len(in.SignDescs) == 0:
		return nil, fmt.Errorf("at least one SignDescs MUST be " +
			"passed in")
	}

	// Now that we know we have an actual transaction to decode, we'll
	// deserialize it into something that we can properly utilize.
	var (
		txToSign wire.MsgTx
		err      error
	)
	txReader := bytes.NewReader(in.RawTxBytes)
	if err := txToSign.Deserialize(txReader); err != nil {
		return nil, fmt.Errorf("unable to decode tx: %v", err)
	}

	var (
		sigHashCache      = input.NewTxSigHashesV0Only(&txToSign)
		prevOutputFetcher = txscript.NewMultiPrevOutFetcher(nil)
	)

	// If we're spending one or more SegWit v1 (Taproot) inputs, then we
	// need the full UTXO information available.
	if len(in.PrevOutputs) > 0 {
		if len(in.PrevOutputs) != len(txToSign.TxIn) {
			return nil, fmt.Errorf("provided previous outputs " +
				"doesn't match number of transaction inputs")
		}

		// Add all previous inputs to our sighash prev out fetcher so we
		// can calculate the sighash correctly.
		for idx, txIn := range txToSign.TxIn {
			prevOutputFetcher.AddPrevOut(
				txIn.PreviousOutPoint, &wire.TxOut{
					Value:    in.PrevOutputs[idx].Value,
					PkScript: in.PrevOutputs[idx].PkScript,
				},
			)
		}
		sigHashCache = txscript.NewTxSigHashes(
			&txToSign, prevOutputFetcher,
		)
	}

	log.Debugf("Generating sigs for %v inputs: ", len(in.SignDescs))

	// With the transaction deserialized, we'll now convert sign descs so
	// we can feed it into the actual signer.
	signDescs := make([]*input.SignDescriptor, 0, len(in.SignDescs))
	for _, signDesc := range in.SignDescs {
		keyDesc := signDesc.KeyDesc

		// The caller can either specify the key using the raw pubkey,
		// or the description of the key. We'll still attempt to parse
		// both if both were provided however, to ensure the underlying
		// SignOutputRaw has as much information as possible.
		var (
			targetPubKey *btcec.PublicKey
			keyLoc       keychain.KeyLocator
		)

		// If this method doesn't return nil, then we know that user is
		// attempting to include a raw serialized pub key.
		if keyDesc.GetRawKeyBytes() != nil {
			targetPubKey, err = parseRawKeyBytes(
				keyDesc.GetRawKeyBytes(),
			)
			if err != nil {
				return nil, err
			}
		}

		// Similarly, if they specified a key locator, then we'll parse
		// that as well.
		if keyDesc.GetKeyLoc() != nil {
			protoLoc := keyDesc.GetKeyLoc()
			keyLoc = keychain.KeyLocator{
				Family: keychain.KeyFamily(
					protoLoc.KeyFamily,
				),
				Index: uint32(protoLoc.KeyIndex),
			}
		}

		// Check what sign method was selected by the user so, we know
		// exactly what we're expecting and can prevent some of the more
		// obvious usage errors.
		signMethod, err := UnmarshalSignMethod(signDesc.SignMethod)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal sign "+
				"method: %v", err)
		}
		if !signMethod.PkScriptCompatible(signDesc.Output.PkScript) {
			return nil, fmt.Errorf("selected sign method %v is "+
				"not compatible with given pk script %x",
				signMethod, signDesc.Output.PkScript)
		}

		// Perform input validation according to the sign method. Not
		// all methods require the same fields to be provided.
		switch signMethod {
		case input.WitnessV0SignMethod:
			// If a witness script isn't passed, then we can't
			// proceed, as in the p2wsh case, we can't properly
			// generate the sighash. A P2WKH doesn't need a witness
			// script. But SignOutputRaw still needs to know the PK
			// script that was used for the output. We'll send it in
			// the WitnessScript field, the SignOutputRaw RPC will
			// know what to do with it when creating the sighash.
			if len(signDesc.WitnessScript) == 0 {
				return nil, fmt.Errorf("witness script MUST " +
					"be specified for segwit v0 sign " +
					"method")
			}

		case input.TaprootKeySpendBIP0086SignMethod:
			if len(signDesc.TapTweak) > 0 {
				return nil, fmt.Errorf("tap tweak must be " +
					"empty for BIP0086 key spend")
			}

		case input.TaprootKeySpendSignMethod:
			if len(signDesc.TapTweak) != sha256.Size {
				return nil, fmt.Errorf("tap tweak must be " +
					"specified for key spend with root " +
					"hash")
			}

		case input.TaprootScriptSpendSignMethod:
			if len(signDesc.WitnessScript) == 0 {
				return nil, fmt.Errorf("witness script MUST " +
					"be specified for taproot script " +
					"spend method")
			}
		}

		// If the users provided a double tweak, then we'll need to
		// parse that out now to ensure their input is properly signed.
		var tweakPrivKey *btcec.PrivateKey
		if len(signDesc.DoubleTweak) != 0 {
			tweakPrivKey, _ = btcec.PrivKeyFromBytes(
				signDesc.DoubleTweak,
			)
		}

		// Finally, with verification and parsing complete, we can
		// construct the final sign descriptor to generate the proper
		// signature for this input.
		signDescs = append(signDescs, &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: keyLoc,
				PubKey:     targetPubKey,
			},
			SingleTweak:   signDesc.SingleTweak,
			DoubleTweak:   tweakPrivKey,
			TapTweak:      signDesc.TapTweak,
			WitnessScript: signDesc.WitnessScript,
			SignMethod:    signMethod,
			Output: &wire.TxOut{
				Value:    signDesc.Output.Value,
				PkScript: signDesc.Output.PkScript,
			},
			HashType:          txscript.SigHashType(signDesc.Sighash),
			SigHashes:         sigHashCache,
			InputIndex:        int(signDesc.InputIndex),
			PrevOutputFetcher: prevOutputFetcher,
		})

		// Are we trying to sign for a Taproot output? Then we need all
		// previous outputs being declared, otherwise we'd run into a
		// panic later on.
		if txscript.IsPayToTaproot(signDesc.Output.PkScript) {
			for idx, txIn := range txToSign.TxIn {
				utxo := prevOutputFetcher.FetchPrevOutput(
					txIn.PreviousOutPoint,
				)
				if utxo == nil {
					return nil, fmt.Errorf("error signing "+
						"taproot output, transaction "+
						"input %d is missing its "+
						"previous outpoint information",
						idx)
				}
			}
		}
	}

	// Now that we've mapped all the proper sign descriptors, we can
	// request signatures for each of them, passing in the transaction to
	// be signed.
	numSigs := len(in.SignDescs)
	resp := &SignResp{
		RawSigs: make([][]byte, numSigs),
	}
	for i, signDesc := range signDescs {
		sig, err := s.cfg.Signer.SignOutputRaw(&txToSign, signDesc)
		if err != nil {
			log.Errorf("unable to generate sig for input "+
				"#%v: %v", i, err)

			return nil, err
		}

		resp.RawSigs[i] = sig.Serialize()
	}

	return resp, nil
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method should be capable of generating the proper input script for both
// regular p2wkh/p2tr outputs and p2wkh outputs nested within a regular p2sh
// output.
//
// Note that when using this method to sign inputs belonging to the wallet, the
// only items of the SignDescriptor that need to be populated are pkScript in
// the TxOut field, the value in that same field, and finally the input index.
func (s *Server) ComputeInputScript(ctx context.Context,
	in *SignReq) (*InputScriptResp, error) {

	switch {
	// If the client doesn't specify a transaction, then there's nothing to
	// sign, so we'll exit early.
	case len(in.RawTxBytes) == 0:
		return nil, fmt.Errorf("a transaction to sign MUST be " +
			"passed in")

	// If the client doesn't tell us *how* to sign the transaction, then we
	// can't sign anything, so we'll exit early.
	case len(in.SignDescs) == 0:
		return nil, fmt.Errorf("at least one SignDescs MUST be " +
			"passed in")
	}

	// Now that we know we have an actual transaction to decode, we'll
	// deserialize it into something that we can properly utilize.
	var txToSign wire.MsgTx
	txReader := bytes.NewReader(in.RawTxBytes)
	if err := txToSign.Deserialize(txReader); err != nil {
		return nil, fmt.Errorf("unable to decode tx: %v", err)
	}

	var (
		sigHashCache      = input.NewTxSigHashesV0Only(&txToSign)
		prevOutputFetcher = txscript.NewMultiPrevOutFetcher(nil)
	)

	// If we're spending one or more SegWit v1 (Taproot) inputs, then we
	// need the full UTXO information available.
	if len(in.PrevOutputs) > 0 {
		if len(in.PrevOutputs) != len(txToSign.TxIn) {
			return nil, fmt.Errorf("provided previous outputs " +
				"doesn't match number of transaction inputs")
		}

		// Add all previous inputs to our sighash prev out fetcher so we
		// can calculate the sighash correctly.
		for idx, txIn := range txToSign.TxIn {
			prevOutputFetcher.AddPrevOut(
				txIn.PreviousOutPoint, &wire.TxOut{
					Value:    in.PrevOutputs[idx].Value,
					PkScript: in.PrevOutputs[idx].PkScript,
				},
			)
		}
		sigHashCache = txscript.NewTxSigHashes(
			&txToSign, prevOutputFetcher,
		)
	}

	signDescs := make([]*input.SignDescriptor, 0, len(in.SignDescs))
	for _, signDesc := range in.SignDescs {
		// For this method, the only fields that we care about are the
		// hash type, and the information concerning the output as we
		// only know how to provide full witnesses for outputs that we
		// solely control.
		signDescs = append(signDescs, &input.SignDescriptor{
			Output: &wire.TxOut{
				Value:    signDesc.Output.Value,
				PkScript: signDesc.Output.PkScript,
			},
			HashType:          txscript.SigHashType(signDesc.Sighash),
			SigHashes:         sigHashCache,
			PrevOutputFetcher: prevOutputFetcher,
			InputIndex:        int(signDesc.InputIndex),
		})
	}

	// With all of our signDescs assembled, we can now generate a valid
	// input script for each of them, and collate the responses to return
	// back to the caller.
	numWitnesses := len(in.SignDescs)
	resp := &InputScriptResp{
		InputScripts: make([]*InputScript, numWitnesses),
	}
	for i, signDesc := range signDescs {
		inputScript, err := s.cfg.Signer.ComputeInputScript(
			&txToSign, signDesc,
		)
		if err != nil {
			return nil, err
		}

		resp.InputScripts[i] = &InputScript{
			Witness:   inputScript.Witness,
			SigScript: inputScript.SigScript,
		}
	}

	return resp, nil
}

// SignMessage signs a message with the key specified in the key locator. The
// returned signature is fixed-size LN wire format encoded.
func (s *Server) SignMessage(_ context.Context,
	in *SignMessageReq) (*SignMessageResp, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("a message to sign MUST be passed in")
	}
	if in.KeyLoc == nil {
		return nil, fmt.Errorf("a key locator MUST be passed in")
	}
	if in.SchnorrSig && in.CompactSig {
		return nil, fmt.Errorf("compact format can not be used for " +
			"Schnorr signatures")
	}

	// Describe the private key we'll be using for signing.
	keyLocator := keychain.KeyLocator{
		Family: keychain.KeyFamily(in.KeyLoc.KeyFamily),
		Index:  uint32(in.KeyLoc.KeyIndex),
	}

	// Use the schnorr signature algorithm to sign the message.
	if in.SchnorrSig {
		sig, err := s.cfg.KeyRing.SignMessageSchnorr(
			keyLocator, in.Msg, in.DoubleHash,
			in.SchnorrSigTapTweak,
		)
		if err != nil {
			return nil, fmt.Errorf("can't sign the hash: %v", err)
		}

		sigParsed, err := schnorr.ParseSignature(sig.Serialize())
		if err != nil {
			return nil, fmt.Errorf("can't parse Schnorr "+
				"signature: %v", err)
		}

		return &SignMessageResp{
			Signature: sigParsed.Serialize(),
		}, nil
	}

	// To allow a watch-only wallet to forward the SignMessageCompact to an
	// endpoint that doesn't add the message prefix, we allow this RPC to
	// also return the compact signature format instead of adding a flag to
	// the lnrpc.SignMessage call that removes the message prefix.
	if in.CompactSig {
		sigBytes, err := s.cfg.KeyRing.SignMessageCompact(
			keyLocator, in.Msg, in.DoubleHash,
		)
		if err != nil {
			return nil, fmt.Errorf("can't sign the hash: %v", err)
		}

		return &SignMessageResp{
			Signature: sigBytes,
		}, nil
	}

	// Create the raw ECDSA signature first and convert it to the final wire
	// format after.
	sig, err := s.cfg.KeyRing.SignMessage(
		keyLocator, in.Msg, in.DoubleHash,
	)
	if err != nil {
		return nil, fmt.Errorf("can't sign the hash: %v", err)
	}
	wireSig, err := lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, fmt.Errorf("can't convert to wire format: %v", err)
	}
	return &SignMessageResp{
		Signature: wireSig.ToSignatureBytes(),
	}, nil
}

// VerifyMessage verifies a signature over a message using the public key
// provided. The signature must be fixed-size LN wire format encoded.
func (s *Server) VerifyMessage(_ context.Context,
	in *VerifyMessageReq) (*VerifyMessageResp, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("a message to verify MUST be passed in")
	}
	if in.Signature == nil {
		return nil, fmt.Errorf("a signature to verify MUST be passed " +
			"in")
	}
	if in.Pubkey == nil {
		return nil, fmt.Errorf("a pubkey to verify MUST be passed in")
	}

	// We allow for Schnorr signatures to be verified.
	if in.IsSchnorrSig {
		// We expect the public key to be in the BIP-340 32-byte format
		// for Schnorr signatures.
		pubkey, err := schnorr.ParsePubKey(in.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("unable to parse pubkey: %v",
				err)
		}

		sigParsed, err := schnorr.ParseSignature(in.Signature)
		if err != nil {
			return nil, fmt.Errorf("can't parse Schnorr "+
				"signature: %v", err)
		}

		digest := chainhash.HashB(in.Msg)
		valid := sigParsed.Verify(digest, pubkey)

		return &VerifyMessageResp{
			Valid: valid,
		}, nil
	}

	pubkey, err := btcec.ParsePubKey(in.Pubkey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse pubkey: %v", err)
	}

	// The signature must be fixed-size LN wire format encoded.
	wireSig, err := lnwire.NewSigFromRawSignature(in.Signature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %v", err)
	}
	sig, err := wireSig.ToSignature()
	if err != nil {
		return nil, fmt.Errorf("failed to convert from wire format: %v",
			err)
	}

	// The signature is over the sha256 hash of the message.
	digest := chainhash.HashB(in.Msg)
	valid := sig.Verify(digest, pubkey)
	return &VerifyMessageResp{
		Valid: valid,
	}, nil
}

// DeriveSharedKey returns a shared secret key by performing Diffie-Hellman key
// derivation between the ephemeral public key in the request and the node's
// key specified in the key_desc parameter. Either a key locator or a raw public
// key is expected in the key_desc, if neither is supplied, defaults to the
// node's identity private key. The old key_loc parameter in the request
// shouldn't be used anymore.
// The resulting shared public key is serialized in the compressed format and
// hashed with sha256, resulting in the final key length of 256bit.
func (s *Server) DeriveSharedKey(_ context.Context, in *SharedKeyRequest) (
	*SharedKeyResponse, error) {

	// Check that EphemeralPubkey is valid.
	ephemeralPubkey, err := parseRawKeyBytes(in.EphemeralPubkey)
	if err != nil {
		return nil, fmt.Errorf("error in ephemeral pubkey: %v", err)
	}
	if ephemeralPubkey == nil {
		return nil, fmt.Errorf("must provide ephemeral pubkey")
	}

	// Check for backward compatibility. The caller either specifies the old
	// key_loc field, or the new key_desc field, but not both.
	if in.KeyDesc != nil && in.KeyLoc != nil {
		return nil, fmt.Errorf("use either key_desc or key_loc")
	}

	// When key_desc is used, the key_desc.key_loc is expected as the caller
	// needs to specify the KeyFamily.
	if in.KeyDesc != nil && in.KeyDesc.KeyLoc == nil {
		return nil, fmt.Errorf("when setting key_desc the field " +
			"key_desc.key_loc must also be set")
	}

	// We extract two params, rawKeyBytes and keyLoc. Notice their initial
	// values will be overwritten if not using the deprecated RPC param.
	var rawKeyBytes []byte
	keyLoc := in.KeyLoc
	if in.KeyDesc != nil {
		keyLoc = in.KeyDesc.GetKeyLoc()
		rawKeyBytes = in.KeyDesc.GetRawKeyBytes()
	}

	// When no keyLoc is supplied, defaults to the node's identity private
	// key.
	if keyLoc == nil {
		keyLoc = &KeyLocator{
			KeyFamily: int32(keychain.KeyFamilyNodeKey),
			KeyIndex:  0,
		}
	}

	// Check the caller is using either the key index or the raw public key
	// to perform the ECDH, we can't have both.
	if rawKeyBytes != nil && keyLoc.KeyIndex != 0 {
		return nil, fmt.Errorf("use either raw_key_bytes or key_index")
	}

	// Check the raw public key is valid. Notice that if the rawKeyBytes is
	// empty, the parseRawKeyBytes won't return an error, a nil
	// *btcec.PublicKey is returned instead.
	pk, err := parseRawKeyBytes(rawKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("error in raw pubkey: %v", err)
	}

	// Create a key descriptor. When the KeyIndex is not specified, it uses
	// the empty value 0, and when the raw public key is not specified, the
	// pk is nil.
	keyDescriptor := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(keyLoc.KeyFamily),
			Index:  uint32(keyLoc.KeyIndex),
		},
		PubKey: pk,
	}

	// Derive the shared key using ECDH and hashing the serialized
	// compressed shared point.
	sharedKeyHash, err := s.cfg.KeyRing.ECDH(keyDescriptor, ephemeralPubkey)
	if err != nil {
		err := fmt.Errorf("unable to derive shared key: %v", err)
		log.Error(err)
		return nil, err
	}

	return &SharedKeyResponse{SharedKey: sharedKeyHash[:]}, nil
}

// MuSig2CombineKeys combines the given set of public keys into a single
// combined MuSig2 combined public key, applying the given tweaks.
func (s *Server) MuSig2CombineKeys(_ context.Context,
	in *MuSig2CombineKeysRequest) (*MuSig2CombineKeysResponse, error) {

	// Check the now mandatory version first. We made the version mandatory,
	// so we don't get unexpected/undefined behavior for old clients that
	// don't specify the version. Since this API is still declared to be
	// experimental this should be the approach that leads to the least
	// amount of unexpected behavior.
	version, err := UnmarshalMuSig2Version(in.Version)
	if err != nil {
		return nil, fmt.Errorf("error parsing version: %w", err)
	}

	// Parse the public keys of all signing participants. This must also
	// include our own, local key.
	allSignerPubKeys, err := input.MuSig2ParsePubKeys(
		version, in.AllSignerPubkeys,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing all signer public "+
			"keys: %w", err)
	}

	// Are there any tweaks to apply to the combined public key?
	tweaks, err := UnmarshalTweaks(in.Tweaks, in.TaprootTweak)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling tweak options: %v",
			err)
	}

	// Combine the keys now without creating a session in memory.
	combinedKey, err := input.MuSig2CombineKeys(
		version, allSignerPubKeys, true, tweaks,
	)
	if err != nil {
		return nil, fmt.Errorf("error combining keys: %v", err)
	}

	var internalKeyBytes []byte
	if combinedKey.PreTweakedKey != nil {
		internalKeyBytes = schnorr.SerializePubKey(
			combinedKey.PreTweakedKey,
		)
	}

	return &MuSig2CombineKeysResponse{
		CombinedKey: schnorr.SerializePubKey(
			combinedKey.FinalKey,
		),
		TaprootInternalKey: internalKeyBytes,
		Version:            in.Version,
	}, nil
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of RPC calls necessary later on.
func (s *Server) MuSig2CreateSession(_ context.Context,
	in *MuSig2SessionRequest) (*MuSig2SessionResponse, error) {

	// Check the now mandatory version first. We made the version mandatory,
	// so we don't get unexpected/undefined behavior for old clients that
	// don't specify the version. Since this API is still declared to be
	// experimental this should be the approach that leads to the least
	// amount of unexpected behavior.
	version, err := UnmarshalMuSig2Version(in.Version)
	if err != nil {
		return nil, fmt.Errorf("error parsing version: %w", err)
	}

	// A key locator is always mandatory.
	if in.KeyLoc == nil {
		return nil, fmt.Errorf("missing key_loc")
	}
	keyLoc := keychain.KeyLocator{
		Family: keychain.KeyFamily(in.KeyLoc.KeyFamily),
		Index:  uint32(in.KeyLoc.KeyIndex),
	}

	// Parse the public keys of all signing participants. This must also
	// include our own, local key.
	allSignerPubKeys, err := input.MuSig2ParsePubKeys(
		version, in.AllSignerPubkeys,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing all signer public "+
			"keys: %w", err)
	}

	// We participate a nonce ourselves, so we can't have more nonces than
	// the total number of participants minus ourselves.
	maxNonces := len(in.AllSignerPubkeys) - 1
	if len(in.OtherSignerPublicNonces) > maxNonces {
		return nil, fmt.Errorf("too many other signer public nonces, "+
			"got %d but expected a maximum of %d",
			len(in.OtherSignerPublicNonces), maxNonces)
	}

	// Parse all other nonces we might already know.
	otherSignerNonces, err := parseMuSig2PublicNonces(
		in.OtherSignerPublicNonces, true,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing other nonces: %v", err)
	}

	// Are there any tweaks to apply to the combined public key?
	tweaks, err := UnmarshalTweaks(in.Tweaks, in.TaprootTweak)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling tweak options: %v",
			err)
	}

	// Register the session with the internal wallet/signer now.
	session, err := s.cfg.Signer.MuSig2CreateSession(
		version, keyLoc, allSignerPubKeys, tweaks, otherSignerNonces,
	)
	if err != nil {
		return nil, fmt.Errorf("error registering session: %v", err)
	}

	var internalKeyBytes []byte
	if session.TaprootTweak {
		internalKeyBytes = schnorr.SerializePubKey(
			session.TaprootInternalKey,
		)
	}

	return &MuSig2SessionResponse{
		SessionId: session.SessionID[:],
		CombinedKey: schnorr.SerializePubKey(
			session.CombinedKey,
		),
		TaprootInternalKey: internalKeyBytes,
		LocalPublicNonces:  session.PublicNonce[:],
		HaveAllNonces:      session.HaveAllNonces,
		Version:            in.Version,
	}, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID.
func (s *Server) MuSig2RegisterNonces(_ context.Context,
	in *MuSig2RegisterNoncesRequest) (*MuSig2RegisterNoncesResponse, error) {

	// Check session ID length.
	sessionID, err := parseMuSig2SessionID(in.SessionId)
	if err != nil {
		return nil, fmt.Errorf("error parsing session ID: %v", err)
	}

	// Parse the other signing participants' nonces. We can't validate the
	// number of nonces here because we don't have access to the session in
	// this context. But the signer will be able to make sure we don't
	// register more nonces than there are signers (which would mean
	// something is wrong in the signing setup). But we want at least a
	// single nonce for each call.
	otherSignerNonces, err := parseMuSig2PublicNonces(
		in.OtherSignerPublicNonces, false,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing other nonces: %v", err)
	}

	// Register the nonces now.
	haveAllNonces, err := s.cfg.Signer.MuSig2RegisterNonces(
		sessionID, otherSignerNonces,
	)
	if err != nil {
		return nil, fmt.Errorf("error registering nonces: %v", err)
	}

	return &MuSig2RegisterNoncesResponse{HaveAllNonces: haveAllNonces}, nil
}

// MuSig2Sign creates a partial signature using the local signing key that was
// specified when the session was created. This can only be called when all
// public nonces of all participants are known and have been registered with
// the session. If this node isn't responsible for combining all the partial
// signatures, then the cleanup flag should be set, indicating that the session
// can be removed from memory once the signature was produced.
func (s *Server) MuSig2Sign(_ context.Context,
	in *MuSig2SignRequest) (*MuSig2SignResponse, error) {

	// Check session ID length.
	sessionID, err := parseMuSig2SessionID(in.SessionId)
	if err != nil {
		return nil, fmt.Errorf("error parsing session ID: %v", err)
	}

	// Schnorr signatures only work reliably if the message is 32 bytes.
	msg := [sha256.Size]byte{}
	if len(in.MessageDigest) != sha256.Size {
		return nil, fmt.Errorf("invalid message digest size, got %d "+
			"but expected %d", len(in.MessageDigest), sha256.Size)
	}
	copy(msg[:], in.MessageDigest)

	// Create our own partial signature with the local signing key.
	partialSig, err := s.cfg.Signer.MuSig2Sign(sessionID, msg, in.Cleanup)
	if err != nil {
		return nil, fmt.Errorf("error signing: %v", err)
	}

	serializedPartialSig, err := input.SerializePartialSignature(partialSig)
	if err != nil {
		return nil, fmt.Errorf("error serializing sig: %v", err)
	}

	return &MuSig2SignResponse{
		LocalPartialSignature: serializedPartialSig[:],
	}, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the local one,
// if it already exists. Once a partial signature of all participants is
// registered, the final signature will be combined and returned.
func (s *Server) MuSig2CombineSig(_ context.Context,
	in *MuSig2CombineSigRequest) (*MuSig2CombineSigResponse, error) {

	// Check session ID length.
	sessionID, err := parseMuSig2SessionID(in.SessionId)
	if err != nil {
		return nil, fmt.Errorf("error parsing session ID: %v", err)
	}

	// Parse all other signatures. This can be called multiple times, so we
	// can't really sanity check how many we already have vs. how many the
	// user supplied in this call.
	partialSigs, err := parseMuSig2PartialSignatures(
		in.OtherPartialSignatures,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing partial signatures: %v",
			err)
	}

	// Combine the signatures now, potentially getting the final, full
	// signature if we've already got all partial ones.
	finalSig, haveAllSigs, err := s.cfg.Signer.MuSig2CombineSig(
		sessionID, partialSigs,
	)
	if err != nil {
		return nil, fmt.Errorf("error combining signatures: %v", err)
	}

	resp := &MuSig2CombineSigResponse{
		HaveAllSignatures: haveAllSigs,
	}

	if haveAllSigs {
		resp.FinalSignature = finalSig.Serialize()
	}

	return resp, err
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (s *Server) MuSig2Cleanup(_ context.Context,
	in *MuSig2CleanupRequest) (*MuSig2CleanupResponse, error) {

	// Check session ID length.
	sessionID, err := parseMuSig2SessionID(in.SessionId)
	if err != nil {
		return nil, fmt.Errorf("error parsing session ID: %v", err)
	}

	err = s.cfg.Signer.MuSig2Cleanup(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error cleaning up session: %v", err)
	}

	return &MuSig2CleanupResponse{}, nil
}

// parseRawKeyBytes checks that the provided raw public key is valid and returns
// the public key. A nil public key is returned if the length of the rawKeyBytes
// is zero.
func parseRawKeyBytes(rawKeyBytes []byte) (*btcec.PublicKey, error) {
	switch {
	case len(rawKeyBytes) == 33:
		// If a proper raw key was provided, then we'll attempt
		// to decode and parse it.
		return btcec.ParsePubKey(rawKeyBytes)

	case len(rawKeyBytes) == 0:
		// No key is provided, return nil.
		return nil, nil

	default:
		// If the user provided a raw key, but it's of the
		// wrong length, then we'll return with an error.
		return nil, fmt.Errorf("pubkey must be " +
			"serialized in compressed format if " +
			"specified")
	}
}

// parseMuSig2SessionID parses a MuSig2 session ID from a raw byte slice.
func parseMuSig2SessionID(rawID []byte) (input.MuSig2SessionID, error) {
	sessionID := input.MuSig2SessionID{}

	// The session ID must be exact in its length.
	if len(rawID) != sha256.Size {
		return sessionID, fmt.Errorf("invalid session ID size, got "+
			"%d but expected %d", len(rawID), sha256.Size)
	}
	copy(sessionID[:], rawID)

	return sessionID, nil
}

// parseMuSig2PublicNonces sanity checks and parses the other signers' public
// nonces.
func parseMuSig2PublicNonces(pubNonces [][]byte,
	emptyAllowed bool) ([][musig2.PubNonceSize]byte, error) {

	// For some calls the nonces are optional while for others it doesn't
	// make any sense to not specify them (for example for the explicit
	// nonce registration call there should be at least one nonce).
	if !emptyAllowed && len(pubNonces) == 0 {
		return nil, fmt.Errorf("at least one other signer public " +
			"nonce is required")
	}

	// Parse all other nonces. This can be called multiple times, so we
	// can't really sanity check how many we already have vs. how many the
	// user supplied in this call.
	otherSignerNonces := make([][musig2.PubNonceSize]byte, len(pubNonces))
	for idx, otherNonceBytes := range pubNonces {
		if len(otherNonceBytes) != musig2.PubNonceSize {
			return nil, fmt.Errorf("invalid public nonce at "+
				"index %d: invalid length, got %d but "+
				"expected %d", idx, len(otherNonceBytes),
				musig2.PubNonceSize)
		}
		copy(otherSignerNonces[idx][:], otherNonceBytes)
	}

	return otherSignerNonces, nil
}

// parseMuSig2PartialSignatures sanity checks and parses the other signers'
// partial signatures.
func parseMuSig2PartialSignatures(
	partialSignatures [][]byte) ([]*musig2.PartialSignature, error) {

	// We always want at least one partial signature.
	if len(partialSignatures) == 0 {
		return nil, fmt.Errorf("at least one partial signature is " +
			"required")
	}

	parsedPartialSigs := make(
		[]*musig2.PartialSignature, len(partialSignatures),
	)
	for idx, otherPartialSigBytes := range partialSignatures {
		sig, err := input.DeserializePartialSignature(
			otherPartialSigBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid partial signature at "+
				"index %d: %v", idx, err)
		}

		parsedPartialSigs[idx] = sig
	}

	return parsedPartialSigs, nil
}

// UnmarshalTweaks parses the RPC tweak descriptions into their native
// counterpart.
func UnmarshalTweaks(rpcTweaks []*TweakDesc,
	taprootTweak *TaprootTweakDesc) (*input.MuSig2Tweaks, error) {

	// Parse the generic tweaks first.
	tweaks := &input.MuSig2Tweaks{
		GenericTweaks: make([]musig2.KeyTweakDesc, len(rpcTweaks)),
	}
	for idx, rpcTweak := range rpcTweaks {
		if len(rpcTweak.Tweak) == 0 {
			return nil, fmt.Errorf("tweak cannot be empty")
		}

		copy(tweaks.GenericTweaks[idx].Tweak[:], rpcTweak.Tweak)
		tweaks.GenericTweaks[idx].IsXOnly = rpcTweak.IsXOnly
	}

	// Now parse the taproot specific tweak.
	if taprootTweak != nil {
		if taprootTweak.KeySpendOnly {
			tweaks.TaprootBIP0086Tweak = true
		} else {
			if len(taprootTweak.ScriptRoot) == 0 {
				return nil, fmt.Errorf("script root cannot " +
					"be empty for non-keyspend")
			}

			tweaks.TaprootTweak = taprootTweak.ScriptRoot
		}
	}

	return tweaks, nil
}

// UnmarshalSignMethod parses the RPC sign method into the native counterpart.
func UnmarshalSignMethod(rpcSignMethod SignMethod) (input.SignMethod, error) {
	switch rpcSignMethod {
	case SignMethod_SIGN_METHOD_WITNESS_V0:
		return input.WitnessV0SignMethod, nil

	case SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND_BIP0086:
		return input.TaprootKeySpendBIP0086SignMethod, nil

	case SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND:
		return input.TaprootKeySpendSignMethod, nil

	case SignMethod_SIGN_METHOD_TAPROOT_SCRIPT_SPEND:
		return input.TaprootScriptSpendSignMethod, nil

	default:
		return 0, fmt.Errorf("unknown RPC sign method <%d>",
			rpcSignMethod)
	}
}
