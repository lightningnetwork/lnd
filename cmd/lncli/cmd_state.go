package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var getStateCommand = cli.Command{
	Name:     "state",
	Category: "Startup",
	Usage:    "Get the current state of the wallet and RPC",
	Description: `
	Get the current state of the wallet. The possible states are:
	 - NON_EXISTING: wallet has not yet been initialized.
	 - LOCKED: wallet is locked.
	 - UNLOCKED: wallet has been unlocked successfully, but the full RPC is
	   not yet ready.
	 - RPC_READY: the daemon has started and the RPC is fully available.
	`,
	Flags:  []cli.Flag{},
	Action: actionDecorator(getState),
}

func getState(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getStateServiceClient(ctx)
	defer cleanUp()

	req := &lnrpc.SubscribeStateRequest{}
	stream, err := client.SubscribeState(ctxb, req)
	if err != nil {
		return err
	}

	// Get a single state, then exit.
	resp, err := stream.Recv()
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
