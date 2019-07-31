// +build !wtclientrpc

package main

import "github.com/urfave/cli"

// wtclientCommands will return nil for non-wtclientrpc builds.
func wtclientCommands() []cli.Command {
	return nil
}
