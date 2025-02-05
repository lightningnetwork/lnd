//go:build !watchtowerrpc
// +build !watchtowerrpc

package commands

import "github.com/urfave/cli/v3"

// watchtowerCommands will return nil for non-watchtowerrpc builds.
func watchtowerCommands() []*cli.Command {
	return nil
}
