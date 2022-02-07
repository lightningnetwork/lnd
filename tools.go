//go:build tools
// +build tools

package lnd

// This file is a workaround to make sure go mod keeps around the btcd
// dependencies in the go.sum file that we only use during integration tests and
// only for certain operating systems. For example, the specific btcd import
// makes sure the indirect dependency github.com/btcsuite/winsvc is kept in the
// go.sum file. Because of the build tag, this dependency never ends up in the
// final lnd binary.
//
// The other imports represent our build tools. Instead of defining a commit we
// want to use for those golang based tools, we use the go mod versioning system
// to unify the way we manage dependencies. So we define our build tool
// dependencies here and pin the version in go.mod.
import (
	_ "github.com/btcsuite/btcd"
	_ "github.com/dvyukov/go-fuzz/go-fuzz"
	_ "github.com/dvyukov/go-fuzz/go-fuzz-build"
	_ "github.com/dvyukov/go-fuzz/go-fuzz-dep"
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
	_ "github.com/ory/go-acc"
	_ "github.com/rinchsan/gosimports/cmd/gosimports"
)
