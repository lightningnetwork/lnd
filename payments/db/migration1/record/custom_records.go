package record

import (
	"fmt"
)

const (
	// CustomTypeStart is the start of the custom tlv type range as defined
	// in BOLT 01.
	CustomTypeStart = 65536
)

// CustomSet stores a set of custom key/value pairs.
type CustomSet map[uint64][]byte

// Validate checks that all custom records are in the custom type range.
func (c CustomSet) Validate() error {
	for key := range c {
		if key < CustomTypeStart {
			return fmt.Errorf("no custom records with types "+
				"below %v allowed", CustomTypeStart)
		}
	}

	return nil
}

// IsKeysend checks if the custom records contain the key send type.
func (c CustomSet) IsKeysend() bool {
	return c[KeySendType] != nil
}
