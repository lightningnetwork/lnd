package commands

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var sendOnionMessageCommand = cli.Command{
	Name:     "sendonionmessage",
	Category: "Peers",
	Usage:    "Send an onion message to a destination node.",
	Description: `
	Send an onion message to the destination node identified by its
	compressed public key. A route is found automatically via graph-based
	pathfinding; a direct send is attempted as a fallback if no graph
	route exists.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest",
			Usage: "hex-encoded compressed public key of the " +
				"destination node",
		},
		cli.StringFlag{
			Name: "final_hop_tlvs",
			Usage: "comma-separated list of TLV records for the " +
				"destination, in the format " +
				"'<type>=<hex_value>,...'. Keys must be >= 64.",
		},
	},
	Action: actionDecorator(sendOnionMessage),
}

// sendOnionMessage sends an onion message to a destination node.
func sendOnionMessage(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	destHex := ctx.String("dest")
	if destHex == "" {
		return errors.New("dest is required")
	}

	dest, err := hex.DecodeString(destHex)
	if err != nil {
		return fmt.Errorf("invalid dest: %w", err)
	}

	req := &lnrpc.SendOnionMessageRequest{
		Destination:  dest,
		FinalHopTlvs: make(map[uint64][]byte),
	}

	if tlvs := ctx.String("final_hop_tlvs"); tlvs != "" {
		for _, record := range strings.Split(tlvs, ",") {
			kv := strings.Split(record, "=")
			if len(kv) != 2 {
				return errors.New("invalid final_hop_tlvs " +
					"format, expected type=hexvalue")
			}

			tlvType, err := strconv.ParseUint(kv[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid TLV type: %w", err)
			}

			val, err := hex.DecodeString(kv[1])
			if err != nil {
				return fmt.Errorf("invalid TLV value: %w", err)
			}

			req.FinalHopTlvs[tlvType] = val
		}
	}

	resp, err := client.SendOnionMessage(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var subscribeOnionMessagesCommand = cli.Command{
	Name:     "subscribeonionmessages",
	Category: "Peers",
	Usage:    "Subscribe to incoming onion messages.",
	Action:   actionDecorator(subscribeOnionMessages),
}

// subscribeOnionMessages subscribes to incoming onion messages.
func subscribeOnionMessages(ctx *cli.Context) error {
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
