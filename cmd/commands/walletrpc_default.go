//go:build !walletrpc
// +build !walletrpc

package commands

import "github.com/urfave/cli/v3"

// walletCommands will return nil for non-walletrpc builds.
func walletCommands() []*cli.Command {
	return nil
}
