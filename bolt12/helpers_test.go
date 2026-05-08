package bolt12

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
)

// bobKey returns the deterministic spec test key for Bob, whose 32-byte scalar
// is 0x42 repeated. Used across signature and round-trip tests so the same key
// is not reconstructed in every callsite.
func bobKey() (*btcec.PrivateKey, *btcec.PublicKey) {
	priv, pub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))

	return priv, pub
}
