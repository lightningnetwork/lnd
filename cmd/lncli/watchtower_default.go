//go:build !watchtowerrpc
// +build !watchtowerrpc

package main

import "github.com/urfave/cli/v2"

// watchtowerCommands will return nil for non-watchtowerrpc builds.
func watchtowerCommands() []*cli.Command {
	return nil
}
