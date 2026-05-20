package lnwire

import (
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
)

// BOLT 4 blinded-path field bounds. Each constant matches the format ceiling
// imposed by the spec encoding (uint8 num_hops, uint16 enclen).
const (
	// pubKeyLen aliases the upstream compressed-pubkey length for shorter
	// usage in this package.
	pubKeyLen = btcec.PubKeyBytesLenCompressed

	// sciddirLen is the on-wire length of a sciddir introduction node
	// (1-byte direction + 8-byte SCID).
	sciddirLen = 9

	// scidLen is the byte length of a short channel ID.
	scidLen = 8

	// maxBlindedPathHops bounds the number of hops a single blinded path
	// may declare. The spec encodes num_hops as a uint8, so 255 is the
	// format's absolute ceiling.
	maxBlindedPathHops = math.MaxUint8

	// maxEncryptedDataLen bounds the encrypted-data field in a single
	// blinded hop. The spec encodes the length as a uint16.
	maxEncryptedDataLen = math.MaxUint16

	// minBlindedHopBytes is the on-wire footprint of the smallest possible
	// blinded hop: BlindedNodeID(33) + enclen(2) + 0 enc_data.
	minBlindedHopBytes = pubKeyLen + 2
)
