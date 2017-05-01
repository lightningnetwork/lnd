package htlcswitch

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/roasbeef/btcutil"
)

// hopID represents the id which is used by propagation subsystem in order to
// identify lightning network node.
// TODO(andrew.shvv) remove after switching to the using channel id.
type hopID [ripemd160.Size]byte

// newHopID creates new instance of hop form node public key.
func newHopID(pubKey []byte) hopID {
	var routeId hopID
	copy(routeId[:], btcutil.Hash160(pubKey))
	return routeId
}

// String returns string representation of hop id.
func (h hopID) String() string {
	return hex.EncodeToString(h[:])
}

// IsEqual checks does the two hop ids are equal.
func (h hopID) IsEqual(h2 hopID) bool {
	return bytes.Equal(h[:], h2[:])
}
