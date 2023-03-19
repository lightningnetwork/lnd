package migration5

import (
	"encoding/binary"
	"fmt"
)

// TowerID is a unique 64-bit identifier allocated to each unique watchtower.
// This allows the client to conserve on-disk space by not needing to always
// reference towers by their pubkey.
type TowerID uint64

// TowerIDFromBytes constructs a TowerID from the provided byte slice. The
// argument must have at least 8 bytes, and should contain the TowerID in
// big-endian byte order.
func TowerIDFromBytes(towerIDBytes []byte) (TowerID, error) {
	if len(towerIDBytes) != 8 {
		return 0, fmt.Errorf("not enough bytes in tower ID. "+
			"Expected 8, got: %d", len(towerIDBytes))
	}

	return TowerID(byteOrder.Uint64(towerIDBytes)), nil
}

// Bytes encodes a TowerID into an 8-byte slice in big-endian byte order.
func (id TowerID) Bytes() []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(id))
	return buf[:]
}

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte
