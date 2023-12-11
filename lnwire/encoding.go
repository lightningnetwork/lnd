package lnwire

// QueryEncoding is an enum-like type that represents exactly how a set data is
// encoded on the wire.
type QueryEncoding uint8

const (
	// EncodingSortedPlain signals that the set of data is encoded using the
	// regular encoding, in a sorted order.
	EncodingSortedPlain QueryEncoding = 0

	// EncodingSortedZlib signals that the set of data is encoded by first
	// sorting the set of channel ID's, as then compressing them using zlib.
	//
	// NOTE: this should no longer be used or accepted.
	EncodingSortedZlib QueryEncoding = 1
)
