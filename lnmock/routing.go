package lnmock

import (
	"bytes"

	"github.com/lightningnetwork/lnd/lnwire"
)

// MockOnion returns a mock onion payload.
func MockOnion() [lnwire.OnionPacketSize]byte {
	var onion [lnwire.OnionPacketSize]byte
	onionBlob := bytes.Repeat([]byte{1}, lnwire.OnionPacketSize)
	copy(onion[:], onionBlob)

	return onion
}
