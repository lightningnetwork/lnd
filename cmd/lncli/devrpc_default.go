//go:build !dev
// +build !dev

package main

import "github.com/urfave/cli"

// devCommands will return nil for non-devrpc builds.
func devCommands() []cli.Command {
	return nil
}
