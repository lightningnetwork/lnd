//go:build bolt12

package commands

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/gijswijs/boltnd/offersrpc"
	"github.com/urfave/cli"
)

var sendOnionMessageCommand = cli.Command{
	Name: "sendonion",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "pubkey",
		},
	},
	Action: actionDecorator(sendOnion),
}

func sendOnion(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	pubkeyFlag := ctx.String("pubkey")
	if pubkeyFlag == "" {
		return errors.New("public key required for onion message")
	}

	pubkeyBytes, err := hex.DecodeString(pubkeyFlag)
	if err != nil {
		return fmt.Errorf("pubkey: %w", err)
	}

	req := &offersrpc.SendOnionMessageRequest{
		Pubkey: pubkeyBytes,
	}

	_, err = client.SendOnionMessage(ctxc, req)
	return err
}

func getOffersClient(ctx *cli.Context) (offersrpc.OffersClient, func()) {
	conn := getClientConn(ctx, true)

	cleanUp := func() {
		conn.Close()
	}

	return offersrpc.NewOffersClient(conn), cleanUp
}

// autopilotCommands will return the set of commands to enable for autopilotrpc
// builds.
func onionMessageCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "onionmessage",
			Category:    "Onion Message",
			Usage:       "Send and receive onion messages.",
			Description: "",
			Subcommands: []cli.Command{
				sendOnionMessageCommand,
			},
		},
	}
}
