package wtdb

import (
	"encoding/hex"

	"github.com/btcsuite/btcd/btcec/v2"
)

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte

// NewSessionIDFromPubKey creates a new SessionID from a public key.
func NewSessionIDFromPubKey(pubKey *btcec.PublicKey) SessionID {
	var sid SessionID
	copy(sid[:], pubKey.SerializeCompressed())
	return sid
}

// String returns a hex encoding of the session id.
func (s SessionID) String() string {
	return hex.EncodeToString(s[:])
}
