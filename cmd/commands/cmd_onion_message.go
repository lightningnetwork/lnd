package commands

import (
	"encoding/hex"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var sendOnionMessageCommand = cli.Command{
	Name:     "sendonionmessage",
	Category: "Peers",
	Usage:    "Send an onion message to a peer",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "peer",
		},
		cli.StringFlag{
			Name: "path_key",
		},
		cli.StringFlag{
			Name: "onion",
		},
	},
	Action: actionDecorator(sendOnionMessage),
}

// sendOnionMessage sends an onion message to a peer.
func sendOnionMessage(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	peer, err := hex.DecodeString(ctx.String("peer"))
	if err != nil {
		return fmt.Errorf("invalid peer hex: %w", err)
	}

	pathKey, err := hex.DecodeString(ctx.String("path_key"))
	if err != nil {
		return fmt.Errorf("invalid path_key hex: %w", err)
	}

	onion, err := hex.DecodeString(ctx.String("onion"))
	if err != nil {
		return fmt.Errorf("invalid onion hex: %w", err)
	}

	resp, err := client.SendOnionMessage(
		ctxc, &lnrpc.SendOnionMessageRequest{
			Peer:    peer,
			PathKey: pathKey,
			Onion:   onion,
		},
	)

	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var subscribeOnionMessageCommand = cli.Command{
	Name:     "subscribeonionmessage",
	Category: "Peers",
	Usage:    "Subscribe to incoming onion messages",
	Action:   actionDecorator(subscribeOnionMessage),
}

// subscribeOnionMessage subscribes to incoming onion messages.
func subscribeOnionMessage(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	stream, err := client.SubscribeOnionMessages(
		ctxc, &lnrpc.SubscribeOnionMessagesRequest{},
	)
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		printRespJSON(msg)
	}
}
