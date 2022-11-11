//go:build buildtagdoesnotexist
// +build buildtagdoesnotexist

package build

// This file is a workaround to make sure go mod keeps around the btcd
// dependencies in the go.sum file that we only use during certain tasks (such
// as integration tests or fuzzing) or only for certain operating systems. For
// example, the specific btcd import makes sure the indirect dependency
// github.com/btcsuite/winsvc is kept in the go.sum file. Because of the build
// tag, this dependency never ends up in the final lnd binary.
import (
	_ "github.com/btcsuite/btcd"
)
