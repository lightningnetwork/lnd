package record

const (
	// CustomTypeStart is the start of the custom tlv type range as defined
	// in BOLT 01.
	CustomTypeStart = 65536
)

// CustomSet stores a set of custom key/value pairs.
type CustomSet map[uint64][]byte
