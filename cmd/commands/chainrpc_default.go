//go:build !chainrpc
// +build !chainrpc

package commands

import "github.com/urfave/cli"

// chainCommands will return nil for non-chainrpc builds.
func chainCommands() []cli.Command {
	return nil
}
