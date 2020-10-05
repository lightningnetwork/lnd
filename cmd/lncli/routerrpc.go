package main

import "github.com/urfave/cli"

// routerCommands returns a list of routerrpc commands.
func routerCommands() []cli.Command {
	return []cli.Command{
		queryMissionControlCommand,
		queryProbCommand,
		resetMissionControlCommand,
		buildRouteCommand,
	}
}
