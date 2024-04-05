package commands

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
	 - WAITING_TO_START: node is waiting to become the leader in a cluster
	   and is not started yet.
	 - NON_EXISTING: wallet has not yet been initialized.
	 - LOCKED: wallet is locked.
	 - UNLOCKED: wallet was unlocked successfully, but RPC server isn't ready.
	 - RPC_ACTIVE: RPC server is active but not fully ready for calls.
	 - SERVER_ACTIVE: RPC server is available and ready to accept calls.
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
