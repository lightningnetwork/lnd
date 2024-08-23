//go:build !autopilotrpc
// +build !autopilotrpc

package commands

import "github.com/urfave/cli"

// autopilotCommands will return nil for non-autopilotrpc builds.
func autopilotCommands() []cli.Command {
	return nil
}
