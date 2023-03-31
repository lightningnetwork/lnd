package migration6

import (
	"encoding/hex"
)

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte

// String returns a hex encoding of the session id.
func (s SessionID) String() string {
	return hex.EncodeToString(s[:])
}
