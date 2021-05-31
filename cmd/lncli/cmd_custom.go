package main

import (
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var sendCustomCommand = cli.Command{
	Name: "sendcustom",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "peer",
		},
		cli.Uint64Flag{
			Name: "type",
		},
		cli.StringFlag{
			Name: "data",
		},
	},
	Action: actionDecorator(sendCustom),
}

func sendCustom(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	peer, err := hex.DecodeString(ctx.String("peer"))
	if err != nil {
		return err
	}

	msgType := ctx.Uint64("type")

	data, err := hex.DecodeString(ctx.String("data"))
	if err != nil {
		return err
	}

	_, err = client.SendCustomMessage(
		ctxc,
		&lnrpc.SendCustomMessageRequest{
			Peer: peer,
			Type: uint32(msgType),
			Data: data,
		},
	)

	return err
}
