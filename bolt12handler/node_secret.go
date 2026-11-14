package bolt12handler

import (
	"encoding/binary"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
)

// nodeSecretPointTag tags the hashes that the node secret point is derived
// from. It is a public domain separator, not a credential.
//
//nolint:gosec
const nodeSecretPointTag = "bolt12/node-secret-point"

// nodeSecretPoint is a fixed point with no known private key. Its ECDH with the
// node key is a secret that only the node can compute. The secrets of this
// package are tagged hashes of it.
var nodeSecretPoint = deriveNodeSecretPoint()

// deriveNodeSecretPoint returns the first point whose x coordinate is the
// tagged hash of a counter. Nobody knows the private key of a point that a
// hash selects.
func deriveNodeSecretPoint() *btcec.PublicKey {
	var counter [4]byte
	for i := uint32(0); ; i++ {
		binary.BigEndian.PutUint32(counter[:], i)
		x := chainhash.TaggedHash(
			[]byte(nodeSecretPointTag), counter[:],
		)

		point, err := schnorr.ParsePubKey(x[:])
		if err == nil {
			return point
		}
	}
}

// ECDHer computes the ECDH of the node key and a public key. It returns the
// SHA256 of the shared point, as keychain.SingleKeyECDH does, so the node key
// can stay in a remote signer.
type ECDHer interface {
	ECDH(pub *btcec.PublicKey) ([32]byte, error)
}
