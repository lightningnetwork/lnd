//go:build !peersrpc
// +build !peersrpc

package commands

import "github.com/urfave/cli/v3"

// peersCommands will return nil for non-peersrpc builds.
func peersCommands() []*cli.Command {
	return nil
}
