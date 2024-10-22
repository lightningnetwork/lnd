package graphdb

import "fmt"

var (
	// ErrUnknownAddressType is returned when a node's addressType is not
	// an expected value.
	ErrUnknownAddressType = fmt.Errorf("address type cannot be resolved")
)
