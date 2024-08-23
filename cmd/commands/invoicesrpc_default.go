//go:build !invoicesrpc
// +build !invoicesrpc

package commands

import "github.com/urfave/cli"

// invoicesCommands will return nil for non-invoicesrpc builds.
func invoicesCommands() []cli.Command {
	return nil
}
