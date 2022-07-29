//go:build !neutrinorpc
// +build !neutrinorpc

package main

import "github.com/urfave/cli/v2"

// neutrinoCommands will return nil for non-neutrinorpc builds.
func neutrinoCommands() []*cli.Command {
	return nil
}
