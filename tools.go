//go:build tools
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

	// Instead of defining a commit we want to use for those golang based
	// tools, we use the go mod versioning system to unify the way we manage
	// dependencies. So we define our build tool dependencies here and pin
	// the version in go.mod.
	_ "github.com/dvyukov/go-fuzz/go-fuzz"
	_ "github.com/dvyukov/go-fuzz/go-fuzz-build"
	_ "github.com/dvyukov/go-fuzz/go-fuzz-dep"
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
	_ "github.com/ory/go-acc"
	_ "golang.org/x/tools/cmd/goimports"
)
