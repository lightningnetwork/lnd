// +build routerrpc

package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"

	"github.com/urfave/cli"
)

var resetMissionControlCommand = cli.Command{
	Name:     "resetmc",
	Category: "Payments",
	Usage:    "Reset internal mission control state.",
	Action:   actionDecorator(resetMissionControl),
}

func resetMissionControl(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.ResetMissionControlRequest{}
	rpcCtx := context.Background()
	_, err := client.ResetMissionControl(rpcCtx, req)
	return err
}
