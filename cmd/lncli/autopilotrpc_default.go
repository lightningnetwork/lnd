//go:build !autopilotrpc
// +build !autopilotrpc

package main

import "github.com/urfave/cli/v2"

// autopilotCommands will return nil for non-autopilotrpc builds.
func autopilotCommands() []*cli.Command {
	return nil
}
