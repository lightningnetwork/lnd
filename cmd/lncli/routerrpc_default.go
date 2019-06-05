// +build !routerrpc

package main

import "github.com/urfave/cli"

// routerCommands will return nil for non-routerrpc builds.
func routerCommands() []cli.Command {
	return nil
}
