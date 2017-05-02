package htlcswitch

import (
	"bytes"
	"encoding/hex"
	"io"

	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/roasbeef/btcutil"
)

// HopID represents the id which is used by propagation subsystem in order to
// identify lightning network node.
// TODO(andrew.shvv) remove after switching to the using channel id.
type HopID [ripemd160.Size]byte

// NewHopID creates new instance of hop form node public key.
func NewHopID(pubKey []byte) HopID {
	var routeID HopID
	copy(routeID[:], btcutil.Hash160(pubKey))
	return routeID
}

// String returns string representation of hop id.
func (h HopID) String() string {
	return hex.EncodeToString(h[:])
}

// IsEqual checks does the two hop ids are equal.
func (h HopID) IsEqual(h2 HopID) bool {
	return bytes.Equal(h[:], h2[:])
}

// HopIterator interface represent the entity which is able to give route
// hops one by one. This interface is used to have an abstraction over the
// algorithm which we use to determine the next hope in htlc route.
type HopIterator interface {
	// Next returns next hop if exist and nil if route is ended.
	Next() *HopID

	// Encode encodes iterator and writes it to the writer.
	Encode(w io.Writer) error
}
