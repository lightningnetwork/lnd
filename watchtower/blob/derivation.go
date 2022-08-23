package blob

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// BreachHintSize is the length of the identifier used to detect remote
// commitment broadcasts.
const BreachHintSize = 16

// BreachHint is the first 16-bytes of SHA256(txid), which is used to identify
// the breach transaction.
type BreachHint [BreachHintSize]byte

// NewBreachHintFromHash creates a breach hint from a transaction ID.
func NewBreachHintFromHash(hash *chainhash.Hash) BreachHint {
	h := sha256.New()
	h.Write(hash[:])

	var hint BreachHint
	copy(hint[:], h.Sum(nil))
	return hint
}

// String returns a hex encoding of the breach hint.
func (h BreachHint) String() string {
	return hex.EncodeToString(h[:])
}

// BreachKey is computed as SHA256(txid || txid), which produces the key for
// decrypting a client's encrypted blobs.
type BreachKey [KeySize]byte

// NewBreachKeyFromHash creates a breach key from a transaction ID.
func NewBreachKeyFromHash(hash *chainhash.Hash) BreachKey {
	h := sha256.New()
	h.Write(hash[:])
	h.Write(hash[:])

	var key BreachKey
	copy(key[:], h.Sum(nil))
	return key
}

// String returns a hex encoding of the breach key.
func (k BreachKey) String() string {
	return hex.EncodeToString(k[:])
}

// NewBreachHintAndKeyFromHash derives a BreachHint and BreachKey from a given
// txid in a single pass. The hint and key are computed as:
//
//	hint = SHA256(txid)
//	key = SHA256(txid || txid)
func NewBreachHintAndKeyFromHash(hash *chainhash.Hash) (BreachHint, BreachKey) {
	var (
		hint BreachHint
		key  BreachKey
	)

	h := sha256.New()
	h.Write(hash[:])
	copy(hint[:], h.Sum(nil))
	h.Write(hash[:])
	copy(key[:], h.Sum(nil))

	return hint, key
}
