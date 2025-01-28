package commands

import "github.com/urfave/cli/v3"

// routerCommands returns a list of routerrpc commands.
func routerCommands() []*cli.Command {
	return []*cli.Command{
		queryMissionControlCommand,
		importMissionControlCommand,
		queryProbCommand,
		resetMissionControlCommand,
		buildRouteCommand,
		getCfgCommand,
		setCfgCommand,
		updateChanStatusCommand,
	}
}
