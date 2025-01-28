//go:build !autopilotrpc
// +build !autopilotrpc

package commands

import "github.com/urfave/cli/v3"

// autopilotCommands will return nil for non-autopilotrpc builds.
func autopilotCommands() []*cli.Command {
	return nil
}
