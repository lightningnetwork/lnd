//go:build autopilotrpc
// +build autopilotrpc

package commands

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/urfave/cli/v3"
)

func getAutopilotClient(cmd *cli.Command) (autopilotrpc.AutopilotClient, func()) {
	conn := getClientConn(cmd, false)

	cleanUp := func() {
		conn.Close()
	}

	return autopilotrpc.NewAutopilotClient(conn), cleanUp
}

var getStatusCommand = &cli.Command{
	Name:        "status",
	Usage:       "Get the active status of autopilot.",
	Description: "",
	Action:      actionDecorator(getStatus),
}

func getStatus(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getAutopilotClient(cmd)
	defer cleanUp()

	req := &autopilotrpc.StatusRequest{}

	resp, err := client.Status(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var enableCommand = &cli.Command{
	Name:        "enable",
	Usage:       "Enable the autopilot.",
	Description: "",
	Action:      actionDecorator(enable),
}

var disableCommand = &cli.Command{
	Name:        "disable",
	Usage:       "Disable the active autopilot.",
	Description: "",
	Action:      actionDecorator(disable),
}

func enable(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getAutopilotClient(cmd)
	defer cleanUp()

	// We will enable the autopilot.
	req := &autopilotrpc.ModifyStatusRequest{
		Enable: true,
	}

	resp, err := client.ModifyStatus(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

func disable(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getAutopilotClient(cmd)
	defer cleanUp()

	// We will disable the autopilot.
	req := &autopilotrpc.ModifyStatusRequest{
		Enable: false,
	}

	resp, err := client.ModifyStatus(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var queryScoresCommand = &cli.Command{
	Name:        "query",
	Usage:       "Query the autopilot heuristics for nodes' scores.",
	ArgsUsage:   "[flags] <pubkey> <pubkey> <pubkey> ...",
	Description: "",
	Action:      actionDecorator(queryScores),
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name: "ignorelocalstate, i",
			Usage: "Ignore local channel state when calculating " +
				"scores.",
		},
	},
}

func queryScores(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getAutopilotClient(cmd)
	defer cleanUp()

	args := cmd.Args().Slice()
	var pubs []string

	// Keep reading pubkeys as long as there are arguments.
	for _, arg := range args {
		pubs = append(pubs, arg)
	}

	req := &autopilotrpc.QueryScoresRequest{
		Pubkeys:          pubs,
		IgnoreLocalState: cmd.Bool("ignorelocalstate"),
	}

	resp, err := client.QueryScores(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

// autopilotCommands will return the set of commands to enable for autopilotrpc
// builds.
func autopilotCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:        "autopilot",
			Category:    "Autopilot",
			Usage:       "Interact with a running autopilot.",
			Description: "",
			Commands: []*cli.Command{
				getStatusCommand,
				enableCommand,
				disableCommand,
				queryScoresCommand,
			},
		},
	}
}
