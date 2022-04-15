//go:build !peersrpc
// +build !peersrpc

package main

import "github.com/urfave/cli"

// peersCommands will return nil for non-peersrpc builds.
func peersCommands() []cli.Command {
	return nil
}
