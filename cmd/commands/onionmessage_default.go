//go:build !bolt12

package commands

import (
	"github.com/urfave/cli"
)

// autopilotCommands will return nil for non-bolt12 builds.
func onionMessageCommands() []cli.Command {
	return nil
}
