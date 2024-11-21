//go:build !neutrinorpc
// +build !neutrinorpc

package commands

import "github.com/urfave/cli"

// neutrinoCommands will return nil for non-neutrinorpc builds.
func neutrinoCommands() []cli.Command {
	return nil
}
