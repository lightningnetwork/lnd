//go:build !walletrpc
// +build !walletrpc

package main

import "github.com/urfave/cli/v2"

// walletCommands will return nil for non-walletrpc builds.
func walletCommands() []*cli.Command {
	return nil
}
