//go:build watchtowerrpc
// +build watchtowerrpc

package commands

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/urfave/cli/v3"
)

func watchtowerCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:     "tower",
			Usage:    "Interact with the watchtower.",
			Category: "Watchtower",
			Commands: []*cli.Command{
				towerInfoCommand,
			},
		},
	}
}

func getWatchtowerClient(cmd *cli.Command) (watchtowerrpc.WatchtowerClient, func()) {
	conn := getClientConn(cmd, false)
	cleanup := func() {
		conn.Close()
	}
	return watchtowerrpc.NewWatchtowerClient(conn), cleanup
}

var towerInfoCommand = &cli.Command{
	Name:   "info",
	Usage:  "Returns basic information related to the active watchtower.",
	Action: actionDecorator(towerInfo),
}

func towerInfo(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	if cmd.NArg() != 0 || cmd.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, cmd, "info")
	}

	client, cleanup := getWatchtowerClient(cmd)
	defer cleanup()

	req := &watchtowerrpc.GetInfoRequest{}
	resp, err := client.GetInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
