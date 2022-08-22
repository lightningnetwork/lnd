package amp

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/lightningnetwork/lnd/lntypes"
)

// Share represents an n-of-n sharing of a secret 32-byte value. The secret can
// be recovered by XORing all n shares together.
type Share [32]byte

// Xor stores the byte-wise xor of shares x and y in z.
func (z *Share) Xor(x, y *Share) {
	for i := range z {
		z[i] = x[i] ^ y[i]
	}
}

// ChildDesc contains the information necessary to derive a child hash/preimage
// pair that is attached to a particular HTLC. This information will be known by
// both the sender and receiver in the process of fulfilling an AMP payment.
type ChildDesc struct {
	// Share is one of n shares of the root seed. Once all n shares are
	// known to the receiver, the Share will also provide entropy to the
	// derivation of child hash and preimage.
	Share Share

	// Index is 32-bit value that can be used to derive up to 2^32 child
	// hashes and preimages from a single Share. This allows the payment
	// hashes sent over the network to be refreshed without needing to
	// modify the Share.
	Index uint32
}

// Child is a payment hash and preimage pair derived from the root seed. In
// addition to the derived values, a Child carries all information required in
// the derivation apart from the root seed (unless n=1).
type Child struct {
	// ChildDesc contains the data required to derive the child hash and
	// preimage below.
	ChildDesc

	// Preimage is the child payment preimage that can be used to settle the
	// HTLC carrying Hash.
	Preimage lntypes.Preimage

	// Hash is the child payment hash that to be carried by the HTLC.
	Hash lntypes.Hash
}

// String returns a human-readable description of a Child.
func (c *Child) String() string {
	return fmt.Sprintf("share=%x, index=%d -> preimage=%v, hash=%v",
		c.Share, c.Index, c.Preimage, c.Hash)
}

// DeriveChild computes the child preimage and child hash for a given (root,
// share, index) tuple. The derivation is defined as:
//
//	child_preimage = SHA256(root || share || be32(index)),
//	child_hash     = SHA256(child_preimage).
func DeriveChild(root Share, desc ChildDesc) *Child {
	var (
		indexBytes [4]byte
		preimage   lntypes.Preimage
		hash       lntypes.Hash
	)

	// Serialize the child index in big-endian order.
	binary.BigEndian.PutUint32(indexBytes[:], desc.Index)

	// Compute child_preimage as SHA256(root || share || child_index).
	h := sha256.New()
	_, _ = h.Write(root[:])
	_, _ = h.Write(desc.Share[:])
	_, _ = h.Write(indexBytes[:])
	copy(preimage[:], h.Sum(nil))

	// Compute child_hash as SHA256(child_preimage).
	h = sha256.New()
	_, _ = h.Write(preimage[:])
	copy(hash[:], h.Sum(nil))

	return &Child{
		ChildDesc: desc,
		Preimage:  preimage,
		Hash:      hash,
	}
}
