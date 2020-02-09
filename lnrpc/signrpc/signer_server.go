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

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
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
	}

	// DefaultSignerMacFilename is the default name of the signer macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultSignerMacFilename = "signer.macaroon"
)

// Server is a sub-server of the main RPC server: the signer RPC. This sub RPC
// server allows external callers to access the full signing capabilities of
// lnd. This allows callers to create custom protocols, external to lnd, even
// backed by multiple distinct lnd across independent failure domains.
type Server struct {
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
	// to see if we need to create it or not.
	macFilePath := cfg.SignerMacPath
	if cfg.MacService != nil && !lnrpc.FileExists(macFilePath) {
		log.Infof("Making macaroons for Signer RPC Server at: %v",
			macFilePath)

		// At this point, we know that the signer macaroon doesn't yet,
		// exist, so we need to create it with the help of the main
		// macaroon service.
		signerMac, err := cfg.MacService.Oven.NewMacaroon(
			context.Background(), bakery.LatestVersion, nil,
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
			os.Remove(macFilePath)
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
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterSignerServer(grpcServer, s)

	log.Debugf("Signer RPC server successfully register with root gRPC " +
		"server")

	return nil
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignReq. If we're unable to find the keys that
// correspond to the KeyLocators in the SignReq then we'll return an error.
// Additionally, if the user doesn't provide the set of required parameters, or
// provides an invalid transaction, then we'll return with an error.
//
// NOTE: The resulting signature should be void of a sighash byte.
func (s *Server) SignOutputRaw(ctx context.Context, in *SignReq) (*SignResp, error) {

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

	sigHashCache := txscript.NewTxSigHashes(&txToSign)

	log.Debugf("Generating sigs for %v inputs: ", len(in.SignDescs))

	// With the transaction deserialized, we'll now convert sign descs so
	// we can feed it into the actual signer.
	signDescs := make([]*input.SignDescriptor, 0, len(in.SignDescs))
	for _, signDesc := range in.SignDescs {
		keyDesc := signDesc.KeyDesc

		// The caller can either specify the key using the raw pubkey,
		// or the description of the key. Below we'll feel out the
		// oneof field to decide which one we will attempt to parse.
		var (
			targetPubKey *btcec.PublicKey
			keyLoc       keychain.KeyLocator
		)
		switch {

		// If this method doesn't return nil, then we know that user is
		// attempting to include a raw serialized pub key.
		case keyDesc.GetRawKeyBytes() != nil:
			rawKeyBytes := keyDesc.GetRawKeyBytes()

			switch {
			// If the user provided a raw key, but it's of the
			// wrong length, then we'll return with an error.
			case len(rawKeyBytes) != 0 && len(rawKeyBytes) != 33:

				return nil, fmt.Errorf("pubkey must be " +
					"serialized in compressed format if " +
					"specified")

			// If a proper raw key was provided, then we'll attempt
			// to decode and parse it.
			case len(rawKeyBytes) != 0 && len(rawKeyBytes) == 33:
				targetPubKey, err = btcec.ParsePubKey(
					rawKeyBytes, btcec.S256(),
				)
				if err != nil {
					return nil, fmt.Errorf("unable to "+
						"parse pubkey: %v", err)
				}
			}

		// Similarly, if they specified a key locator, then we'll use
		// that instead.
		case keyDesc.GetKeyLoc() != nil:
			protoLoc := keyDesc.GetKeyLoc()
			keyLoc = keychain.KeyLocator{
				Family: keychain.KeyFamily(
					protoLoc.KeyFamily,
				),
				Index: uint32(protoLoc.KeyIndex),
			}
		}

		// If a witness script isn't passed, then we can't proceed, as
		// in the p2wsh case, we can't properly generate the sighash.
		if len(signDesc.WitnessScript) == 0 {
			// TODO(roasbeef): if regualr p2wkh, then at times
			// internally we allow script to go by
			return nil, fmt.Errorf("witness script MUST be " +
				"specified")
		}

		// If the users provided a double tweak, then we'll need to
		// parse that out now to ensure their input is properly signed.
		var tweakPrivKey *btcec.PrivateKey
		if len(signDesc.DoubleTweak) != 0 {
			tweakPrivKey, _ = btcec.PrivKeyFromBytes(
				btcec.S256(), signDesc.DoubleTweak,
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
			WitnessScript: signDesc.WitnessScript,
			Output: &wire.TxOut{
				Value:    signDesc.Output.Value,
				PkScript: signDesc.Output.PkScript,
			},
			HashType:   txscript.SigHashType(signDesc.Sighash),
			SigHashes:  sigHashCache,
			InputIndex: int(signDesc.InputIndex),
		})
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

		resp.RawSigs[i] = sig
	}

	return resp, nil
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method should be capable of generating the proper input script for both
// regular p2wkh output and p2wkh outputs nested within a regular p2sh output.
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

	sigHashCache := txscript.NewTxSigHashes(&txToSign)

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
			HashType:   txscript.SigHashType(signDesc.Sighash),
			SigHashes:  sigHashCache,
			InputIndex: int(signDesc.InputIndex),
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
func (s *Server) SignMessage(ctx context.Context,
	in *SignMessageReq) (*SignMessageResp, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("a message to sign MUST be passed in")
	}
	if in.KeyLoc == nil {
		return nil, fmt.Errorf("a key locator MUST be passed in")
	}

	// Derive the private key we'll be using for signing.
	keyLocator := keychain.KeyLocator{
		Family: keychain.KeyFamily(in.KeyLoc.KeyFamily),
		Index:  uint32(in.KeyLoc.KeyIndex),
	}
	privKey, err := s.cfg.KeyRing.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: keyLocator,
	})
	if err != nil {
		return nil, fmt.Errorf("can't derive private key: %v", err)
	}

	// The signature is over the sha256 hash of the message.
	digest := chainhash.HashB(in.Msg)

	// Create the raw ECDSA signature first and convert it to the final wire
	// format after.
	sig, err := privKey.Sign(digest)
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
func (s *Server) VerifyMessage(ctx context.Context,
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
	pubkey, err := btcec.ParsePubKey(in.Pubkey, btcec.S256())
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
// key specified in the key_loc parameter (or the node's identity private key
// if no key locator is specified):
//     P_shared = privKeyNode * ephemeralPubkey
// The resulting shared public key is serialized in the compressed format and
// hashed with sha256, resulting in the final key length of 256bit.
func (s *Server) DeriveSharedKey(_ context.Context, in *SharedKeyRequest) (
	*SharedKeyResponse, error) {

	if len(in.EphemeralPubkey) != 33 {
		return nil, fmt.Errorf("ephemeral pubkey must be " +
			"serialized in compressed format")
	}
	ephemeralPubkey, err := btcec.ParsePubKey(
		in.EphemeralPubkey, btcec.S256(),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to parse pubkey: %v", err)
	}

	// By default, use the node identity private key.
	locator := keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
		Index:  0,
	}
	if in.KeyLoc != nil {
		locator.Family = keychain.KeyFamily(in.KeyLoc.KeyFamily)
		locator.Index = uint32(in.KeyLoc.KeyIndex)
	}

	// Derive our node's private key from the key ring.
	idPrivKey, err := s.cfg.KeyRing.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: locator,
	})
	if err != nil {
		err := fmt.Errorf("unable to derive node private key: %v", err)
		log.Error(err)
		return nil, err
	}
	idPrivKey.Curve = btcec.S256()

	// Derive the shared key using ECDH and hashing the serialized
	// compressed shared point.
	sharedKeyHash := ecdh(ephemeralPubkey, idPrivKey)
	return &SharedKeyResponse{SharedKey: sharedKeyHash}, nil
}

// ecdh performs an ECDH operation between pub and priv. The returned value is
// the sha256 of the compressed shared point.
func ecdh(pub *btcec.PublicKey, priv *btcec.PrivateKey) []byte {
	s := &btcec.PublicKey{}
	x, y := btcec.S256().ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	s.X = x
	s.Y = y

	h := sha256.Sum256(s.SerializeCompressed())
	return h[:]
}
