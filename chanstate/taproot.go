package chanstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/shachain"
)

const (
	// AbsoluteThawHeightThreshold is the threshold at which a thaw height
	// begins to be interpreted as an absolute block height, rather than a
	// relative one.
	AbsoluteThawHeightThreshold uint32 = 500000
)

var (
	// taprootRevRootKey is the key used to derive the revocation root for
	// the taproot nonces. This is done via HMAC of the existing revocation
	// root.
	taprootRevRootKey = []byte("taproot-rev-root")
)

// DeriveMusig2Shachain derives a shachain producer for the taproot channel
// from normal shachain revocation root.
func DeriveMusig2Shachain(revRoot shachain.Producer) (shachain.Producer, error) { //nolint:ll
	// In order to obtain the revocation root hash to create the taproot
	// revocation, we'll encode the producer into a buffer, then use that
	// to derive the shachain root needed.
	var rootHashBuf bytes.Buffer
	if err := revRoot.Encode(&rootHashBuf); err != nil {
		return nil, fmt.Errorf("unable to encode producer: %w", err)
	}

	revRootHash := chainhash.HashH(rootHashBuf.Bytes())

	// For taproot channel types, we'll also generate a distinct shachain
	// root using the same seed information. We'll use this to generate
	// verification nonces for the channel. We'll bind with this a simple
	// hmac.
	taprootRevHmac := hmac.New(sha256.New, taprootRevRootKey)
	if _, err := taprootRevHmac.Write(revRootHash[:]); err != nil {
		return nil, err
	}

	taprootRevRoot := taprootRevHmac.Sum(nil)

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	return shachain.NewRevocationProducerFromBytes(
		taprootRevRoot,
	)
}

// NewMusigVerificationNonce generates the local or verification nonce for
// another musig2 session. In order to permit our implementation to not have to
// write any secret nonce state to disk, we'll use the _next_ shachain
// pre-image as our primary randomness source. When used to generate the nonce
// again to broadcast our commitment hte current height will be used.
func NewMusigVerificationNonce(pubKey *btcec.PublicKey, targetHeight uint64,
	shaGen shachain.Producer) (*musig2.Nonces, error) {

	// Now that we know what height we need, we'll grab the shachain
	// pre-image at the target destination.
	nextPreimage, err := shaGen.AtIndex(targetHeight)
	if err != nil {
		return nil, err
	}

	shaChainRand := musig2.WithCustomRand(bytes.NewBuffer(nextPreimage[:]))
	pubKeyOpt := musig2.WithPublicKey(pubKey)

	return musig2.GenNonces(pubKeyOpt, shaChainRand)
}
