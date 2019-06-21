// +build watchtowerrpc

package main

import (
	"context"
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/urfave/cli"
)

func watchtowerCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "tower",
			Usage:    "Interact with the watchtower.",
			Category: "Watchtower",
			Subcommands: []cli.Command{
				towerInfoCommand,
			},
		},
	}
}

func getWatchtowerClient(ctx *cli.Context) (watchtowerrpc.WatchtowerClient, func()) {
	conn := getClientConn(ctx, false)
	cleanup := func() {
		conn.Close()
	}
	return watchtowerrpc.NewWatchtowerClient(conn), cleanup
}

var towerInfoCommand = cli.Command{
	Name:   "info",
	Usage:  "Returns basic information related to the active watchtower.",
	Action: actionDecorator(towerInfo),
}

func towerInfo(ctx *cli.Context) error {
	if ctx.NArg() != 0 || ctx.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, "info")
	}

	client, cleanup := getWatchtowerClient(ctx)
	defer cleanup()

	req := &watchtowerrpc.GetInfoRequest{}
	resp, err := client.GetInfo(context.Background(), req)
	if err != nil {
		return err
	}

	printJSON(struct {
		Pubkey    string   `json:"pubkey"`
		Listeners []string `json:"listeners"`
		URIs      []string `json:"uris"`
	}{
		Pubkey:    hex.EncodeToString(resp.Pubkey),
		Listeners: resp.Listeners,
		URIs:      resp.Uris,
	})

	return nil
}
