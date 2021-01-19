package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var getCfgCommand = cli.Command{
	Name:  "getmccfg",
	Usage: "Display mission control's config.",
	Description: `
	Returns the config currently being used by mission control.
	`,
	Action: actionDecorator(getCfg),
}

func getCfg(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	ctxb := context.Background()
	resp, err := client.GetMissionControlConfig(
		ctxb, &routerrpc.GetMissionControlConfigRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
