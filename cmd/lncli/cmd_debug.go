package main

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var getDebugInfoCommand = cli.Command{
	Name:     "getdebuginfo",
	Category: "Debug",
	Usage:    "Returns debug information related to the active daemon.",
	Action:   actionDecorator(getDebugInfo),
}

func getDebugInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetDebugInfoRequest{}
	resp, err := client.GetDebugInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
