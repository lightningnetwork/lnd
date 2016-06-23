package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Roasbeef/btcd/wire"
	"github.com/codegangsta/cli"
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
)

// TODO(roasbeef): cli logic for supporting both positional and unix style
// arguments.

func printRespJson(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	// TODO(roasbeef): disable 'omitempty' like behavior

	var out bytes.Buffer
	json.Indent(&out, b, "", "\t")
	out.WriteTo(os.Stdout)
}

var ShellCommand = cli.Command{
	Name:  "shell",
	Usage: "enter interactive shell",
	Action: func(c *cli.Context) {
		println("not implemented yet")
	},
}

var NewAddressCommand = cli.Command{
	Name:   "newaddress",
	Usage:  "generates a new address. Three address types are supported: p2wkh, np2wkh, p2pkh",
	Action: newAddress,
}

func newAddress(ctx *cli.Context) {
	client := getClient(ctx)

	stringAddrType := ctx.Args().Get(0)

	// Map the string encoded address type, to the concrete typed address
	// type enum. An unrecognized address type will result in an error.
	var addrType lnrpc.NewAddressRequest_AddressType
	switch stringAddrType { // TODO(roasbeef): make them ints on the cli?
	case "p2wkh":
		addrType = lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH
	case "np2wkh":
		addrType = lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH
	case "p2pkh":
		addrType = lnrpc.NewAddressRequest_PUBKEY_HASH
	default:
		fatal(fmt.Errorf("invalid address type %v, support address type "+
			"are: p2wkh, np2wkh, p2pkh", stringAddrType))
	}

	ctxb := context.Background()
	addr, err := client.NewAddress(ctxb, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	if err != nil {
		fatal(err)
	}

	printRespJson(addr)
}

var SendManyCommand = cli.Command{
	Name: "sendmany",
	Usage: "create and broadcast a transaction paying the specified " +
		"amount(s) to the passed address(es)",
	Action: sendMany,
}

func sendMany(ctx *cli.Context) {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().Get(0)
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		fatal(err)
	}

	ctxb := context.Background()
	client := getClient(ctx)

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{amountToAddr})
	if err != nil {
		fatal(err)
	}

	printRespJson(txid)
}

var ConnectCommand = cli.Command{
	Name:   "connect",
	Usage:  "connect to a remote lnd peer: <lnid>@host",
	Action: connectPeer,
}

func connectPeer(ctx *cli.Context) {
	ctxb := context.Background()
	client := getClient(ctx)

	targetAddress := ctx.Args().Get(0)
	splitAddr := strings.Split(targetAddress, "@")
	addr := &lnrpc.LightningAddress{
		PubKeyHash: splitAddr[0],
		Host:       splitAddr[1],
	}
	req := &lnrpc.ConnectPeerRequest{addr}

	lnid, err := client.ConnectPeer(ctxb, req)
	if err != nil {
		fatal(err)
	}

	printRespJson(lnid)
}

// TODO(roasbeef): default number of confirmations
var OpenChannelCommand = cli.Command{
	Name: "openchannel",
	Description: "Attempt to open a new channel to an existing peer, " +
		"blocking until the channel is 'open'. Once the channel is " +
		"open, a channelPoint (txid:vout) of the funding output is " +
		"returned.",
	Usage: "openchannel --peer_id=X --local_amt=N --remote_amt=N --num_confs=N",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "peer_id",
			Usage: "the id of the peer to open a channel with",
		},
		cli.IntFlag{
			Name:  "local_amt",
			Usage: "the number of satoshis the wallet should commit to the channel",
		},
		cli.IntFlag{
			Name:  "remote_amt",
			Usage: "the number of satoshis the remote peer should commit to the channel",
		},
		cli.IntFlag{
			Name: "num_confs",
			Usage: "the number of confirmations required before the " +
				"channel is considered 'open'",
		},
	},
	Action: openChannel,
}

func openChannel(ctx *cli.Context) {
	// TODO(roasbeef): add deadline to context
	ctxb := context.Background()
	client := getClient(ctx)

	req := &lnrpc.OpenChannelRequest{
		TargetPeerId:        int32(ctx.Int("peer_id")),
		LocalFundingAmount:  int64(ctx.Int("local_amt")),
		RemoteFundingAmount: int64(ctx.Int("remote_amt")),
		NumConfs:            uint32(ctx.Int("num_confs")),
	}

	resp, err := client.OpenChannel(ctxb, req)
	if err != nil {
		fatal(err)
	}

	txid, err := wire.NewShaHash(resp.ChannelPoint.FundingTxid)
	if err != nil {
		fatal(err)
	}

	index := resp.ChannelPoint.OutputIndex
	printRespJson(struct {
		ChannelPoint string `json:"channel_point"`
	}{
		ChannelPoint: fmt.Sprintf("%v:%v", txid, index),
	},
	)
}

// TODO(roasbeef): also allow short relative channel ID.
var CloseChannelCommand = cli.Command{
	Name: "closechannel",
	Description: "Close an existing channel. The channel can be closed either " +
		"cooperatively, or uncooperatively (forced).",
	Usage: "closechannel funding_txid output_index time_limit allow_force",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "funding_txid",
			Usage: "the txid of the channel's funding transaction",
		},
		cli.IntFlag{
			Name: "output_index",
			Usage: "the output index for the funding output of the funding " +
				"transaction",
		},
		cli.StringFlag{
			Name: "time_limit",
			Usage: "a relative deadline afterwhich the attempt should be " +
				"abandonded",
		},
		cli.BoolFlag{
			Name: "force",
			Usage: "after the time limit has passed, attempted an " +
				"uncooperative closure",
		},
	},
	Action: closeChannel,
}

func closeChannel(ctx *cli.Context) {
	ctxb := context.Background()
	client := getClient(ctx)

	txid, err := wire.NewShaHashFromStr(ctx.String("funding_txid"))
	if err != nil {
		fatal(err)
	}

	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: txid[:],
			OutputIndex: uint32(ctx.Int("output_index")),
		},
	}

	resp, err := client.CloseChannel(ctxb, req)
	if err != nil {
		fatal(err)
	}

	printRespJson(resp)
}

var ListPeersCommand = cli.Command{
	Name:        "listpeers",
	Description: "List all active, currently connected peers.",
	Action:      listPeers,
}

func listPeers(ctx *cli.Context) {
	ctxb := context.Background()
	client := getClient(ctx)

	req := &lnrpc.ListPeersRequest{}
	resp, err := client.ListPeers(ctxb, req)
	if err != nil {
		fatal(err)
	}

	printRespJson(resp)
}

var WalletBalanceCommand = cli.Command{
	Name:        "walletbalance",
	Description: "compute and display the wallet's current balance",
	Usage:       "walletbalance --witness_only=[true|false]",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "witness_only",
			Usage: "if only witness outputs should be considered when " +
				"calculating the wallet's balance",
		},
	},
	Action: walletBalance,
}

func walletBalance(ctx *cli.Context) {
	ctxb := context.Background()
	client := getClient(ctx)

	req := &lnrpc.WalletBalanceRequest{
		WitnessOnly: ctx.Bool("witness_only"),
	}
	resp, err := client.WalletBalance(ctxb, req)
	if err != nil {
		fatal(err)
	}

	printRespJson(resp)
}
