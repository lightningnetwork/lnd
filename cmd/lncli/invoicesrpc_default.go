//go:build !invoicesrpc
// +build !invoicesrpc

package main

import "github.com/urfave/cli/v2"

// invoicesCommands will return nil for non-invoicesrpc builds.
func invoicesCommands() []*cli.Command {
	return nil
}
