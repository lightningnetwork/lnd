package main

import (
	"crypto/sha256"
	"math/big"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

type LnEndpoint string
type LnAddr btcutil.Address
type NodePubKey *btcec.PublicKey

type SharedSecret wire.ShaHash

var zeroNode wire.ShaHash

type MixHeader []byte

// SphinxHeader...
type SphinxHeader struct {
}

// GenerateSphinxHeader...
// TODO(roasbeef): or pass in identifiers as payment path? have map from id -> pubkey
func GenerateSphinxHeader(dest LnAddr, identifier wire.ShaHash, paymentPath []NodePubKey) (*MixHeader, []SharedSecret, error) {
	// Each hop performs ECDH with our ephemeral key pair to arrive at a
	// shared secret. Additionally, each hop randomizes the group element
	// for the next hop by multiplying it by the blinding factor. This way
	// we only need to transmit a single group element, and hops can't link
	// a session back to us if they have several nodes in the path.
	numHops := len(paymentPath)
	hopEphemeralPubKeys := make([]*btcec.PublicKey, numHops)
	hopSharedSecret := make([][sha256.Size]byte, numHops)
	hopBlindingFactors := make([][sha256.Size]byte, numHops)

	// Generate a new ephemeral key to use for ECDH for this session.
	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Compute the triplet for the first hop outside of the main loop.
	// Within the loop each new triplet will be computed recursively based
	// off of the blinding factor of the last hop.
	hopEphemeralPubKeys[0] = sessionKey.PubKey()
	hopSharedSecret[0] = sha256.Sum256(btcec.GenerateSharedSecret(sessionKey, paymentPath[0]))
	hopBlindingFactors[0] = computeBlindingFactor(hopEphemeralPubKeys[0], hopSharedSecret[0][:])

	// x * b_{0} mod n
	cummulativeBlind := new(big.Int).Mul(sessionKey.X, new(big.Int).SetBytes(hopBlindingFactors[0][:]))
	cummulativeBlind.Mod(cummulativeBlind, btcec.S256().N)

	// Now recursively compute the ephemeral ECDH pub keys, the shared
	// secret, and blinding factor for each hop.
	for i := 1; i < numHops-1; i++ {
		// a_{n} = a_{n-1} x c_{n-1}
		// hopEphemeralPubKeys[i] = ScalarBaseMult(cummulativeBlind.Bytes())
		hopEphemeralPubKeys[i] = blindGroupElement(hopEphemeralPubKeys[i-1], cummulativeBlind.Bytes())

		// s_{n} = sha256( y_{n} x c_{n-1} )
		hopSharedSecret[i] = sha256.Sum256(blindGroupElement(paymentPath[i], cummulativeBlind.Bytes()).X.Bytes())

		// b_{n} = sha256(a_{n} || s_{n})
		hopBlindingFactors[i] = computeBlindingFactor(hopEphemeralPubKeys[i], hopSharedSecret[i][:])

		// c_{n} = c_{n-1} * b_{n} mod curve_order
		cummulativeBlind.Mul(cummulativeBlind, new(big.Int).SetBytes(hopBlindingFactors[i][:]))
		cummulativeBlind.Mod(cummulativeBlind, btcec.S256().N)
	}

	return nil, nil, nil
}

// ComputeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey, hopSharedSecret []byte) [sha256.Size]byte {
	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash [sha256.Size]byte
	copy(hash[:], sha.Sum(nil))
	return hash
}

// blindGroupElement blinds the group element by performing scalar
// multiplication of the group element by blindingFactor: G x blindingFactor.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor []byte) *btcec.PublicKey {
	newX, newY := hopPubKey.Curve.ScalarMult(hopPubKey.X, hopPubKey.Y, blindingFactor[:])
	return &btcec.PublicKey{hopPubKey.Curve, newX, newY}
}

// SphinxPayload...
type SphinxPayload struct {
}

// SphinxPacket...
type SphinxPacket struct {
}
