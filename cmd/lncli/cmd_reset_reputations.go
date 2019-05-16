// +build routerrpc

package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"

	"github.com/urfave/cli"
)

var resetReputationsCommand = cli.Command{
	Name:     "resetreputations",
	Category: "Payments",
	Usage:    "Clear reputation data.",
	Action:   actionDecorator(resetReputations),
}

func resetReputations(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.ResetReputationsRequest{}
	rpcCtx := context.Background()
	_, err := client.ResetReputations(rpcCtx, req)
	return err
}
