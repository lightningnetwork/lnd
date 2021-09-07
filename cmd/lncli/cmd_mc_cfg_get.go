package main

import (
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var getCfgCommand = cli.Command{
	Name:     "getmccfg",
	Category: "Mission Control",
	Usage:    "Display mission control's config.",
	Description: `
	Returns the config currently being used by mission control.
	`,
	Action: actionDecorator(getCfg),
}

func getCfg(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	resp, err := client.GetMissionControlConfig(
		ctxc, &routerrpc.GetMissionControlConfigRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
