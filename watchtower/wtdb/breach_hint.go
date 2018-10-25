package wtdb

import (
	"encoding/hex"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// BreachHintSize is the length of the txid prefix used to identify remote
// commitment broadcasts.
const BreachHintSize = 16

// BreachHint is the first 16-bytes of the txid belonging to a revoked
// commitment transaction.
type BreachHint [BreachHintSize]byte

// NewBreachHintFromHash creates a breach hint from a transaction ID.
func NewBreachHintFromHash(hash *chainhash.Hash) BreachHint {
	var hint BreachHint
	copy(hint[:], hash[:BreachHintSize])
	return hint
}

// String returns a hex encoding of the breach hint.
func (h BreachHint) String() string {
	return hex.EncodeToString(h[:])
}
