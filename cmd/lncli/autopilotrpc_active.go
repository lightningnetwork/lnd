// +build autopilotrpc

package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/urfave/cli"
)

func getAutopilotClient(ctx *cli.Context) (autopilotrpc.AutopilotClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return autopilotrpc.NewAutopilotClient(conn), cleanUp
}

var getStatusCommand = cli.Command{
	Name:        "status",
	Usage:       "Get the active status of autopilot.",
	Description: "",
	Action:      actionDecorator(getStatus),
}

func getStatus(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	req := &autopilotrpc.StatusRequest{}

	resp, err := client.Status(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var enableCommand = cli.Command{
	Name:        "enable",
	Usage:       "Enable the autopilot.",
	Description: "",
	Action:      actionDecorator(enable),
}

var disableCommand = cli.Command{
	Name:        "disable",
	Usage:       "Disable the active autopilot.",
	Description: "",
	Action:      actionDecorator(disable),
}

func enable(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	// We will enable the autopilot.
	req := &autopilotrpc.ModifyStatusRequest{
		Enable: true,
	}

	resp, err := client.ModifyStatus(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

func disable(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	// We will disable the autopilot.
	req := &autopilotrpc.ModifyStatusRequest{
		Enable: false,
	}

	resp, err := client.ModifyStatus(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

// autopilotCommands will return the set of commands to enable for autopilotrpc
// builds.
func autopilotCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "autopilot",
			Category:    "Autopilot",
			Usage:       "Interact with a running autopilot.",
			Description: "",
			Subcommands: []cli.Command{
				getStatusCommand,
				enableCommand,
				disableCommand,
			},
		},
	}
}
