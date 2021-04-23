// +build tools

package lnd

import (
	// This is a workaround to make sure go mod keeps around the btcd
	// dependencies in the go.sum file that we only use during integration
	// tests and only for certain operating systems. For example, this
	// specific import makes sure the indirect dependency
	// github.com/btcsuite/winsvc is kept in the go.sum file. Because of the
	// build tag, this dependency never ends up in the final lnd binary.
	_ "github.com/btcsuite/btcd"
)
