//go:build !chainrpc
// +build !chainrpc

package commands

import "github.com/urfave/cli/v3"

// chainCommands will return nil for non-chainrpc builds.
func chainCommands() []*cli.Command {
	return nil
}
