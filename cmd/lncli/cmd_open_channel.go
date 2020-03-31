package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

// TODO(roasbeef): change default number of confirmations
var openChannelCommand = cli.Command{
	Name:     "openchannel",
	Category: "Channels",
	Usage:    "Open a channel to a node or an existing peer.",
	Description: `
	Attempt to open a new channel to an existing peer with the key node-key
	optionally blocking until the channel is 'open'.

	One can also connect to a node before opening a new channel to it by
	setting its host:port via the --connect argument. For this to work,
	the node_key must be provided, rather than the peer_id. This is optional.

	The channel will be initialized with local-amt satoshis local and push-amt
	satoshis for the remote node. Note that specifying push-amt means you give that
	amount to the remote node as part of the channel opening. Once the channel is open,
	a channelPoint (txid:vout) of the funding output is returned.

	If the remote peer supports the option upfront shutdown feature bit (query 
	listpeers to see their supported feature bits), an address to enforce
	payout of funds on cooperative close can optionally be provided. Note that
	if you set this value, you will not be able to cooperatively close out to
	another address.

	One can manually set the fee to be used for the funding transaction via either
	the --conf_target or --sat_per_byte arguments. This is optional.`,
	ArgsUsage: "node-key local-amt push-amt",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "node_key",
			Usage: "the identity public key of the target node/peer " +
				"serialized in compressed format",
		},
		cli.StringFlag{
			Name:  "connect",
			Usage: "(optional) the host:port of the target node",
		},
		cli.IntFlag{
			Name:  "local_amt",
			Usage: "the number of satoshis the wallet should commit to the channel",
		},
		cli.IntFlag{
			Name: "push_amt",
			Usage: "the number of satoshis to give the remote side " +
				"as part of the initial commitment state, " +
				"this is equivalent to first opening a " +
				"channel and sending the remote party funds, " +
				"but done all in one step",
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block and wait until the channel is fully open",
		},
		cli.Int64Flag{
			Name: "conf_target",
			Usage: "(optional) the number of blocks that the " +
				"transaction *should* confirm in, will be " +
				"used for fee estimation",
		},
		cli.Int64Flag{
			Name: "sat_per_byte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/byte that should be used when crafting " +
				"the transaction",
		},
		cli.BoolFlag{
			Name: "private",
			Usage: "make the channel private, such that it won't " +
				"be announced to the greater network, and " +
				"nodes other than the two channel endpoints " +
				"must be explicitly told about it to be able " +
				"to route through it",
		},
		cli.Int64Flag{
			Name: "min_htlc_msat",
			Usage: "(optional) the minimum value we will require " +
				"for incoming HTLCs on the channel",
		},
		cli.Uint64Flag{
			Name: "remote_csv_delay",
			Usage: "(optional) the number of blocks we will require " +
				"our channel counterparty to wait before accessing " +
				"its funds in case of unilateral close. If this is " +
				"not set, we will scale the value according to the " +
				"channel size",
		},
		cli.Uint64Flag{
			Name: "min_confs",
			Usage: "(optional) the minimum number of confirmations " +
				"each one of your outputs used for the funding " +
				"transaction must satisfy",
			Value: 1,
		},
		cli.StringFlag{
			Name: "close_address",
			Usage: "(optional) an address to enforce payout of our " +
				"funds to on cooperative close. Note that if this " +
				"value is set on channel open, you will *not* be " +
				"able to cooperatively close to a different address.",
		},
	},
	Action: actionDecorator(openChannel),
}

func openChannel(ctx *cli.Context) error {
	// TODO(roasbeef): add deadline to context
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()
	var err error

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "openchannel")
		return nil
	}

	minConfs := int32(ctx.Uint64("min_confs"))
	req := &lnrpc.OpenChannelRequest{
		TargetConf:       int32(ctx.Int64("conf_target")),
		SatPerByte:       ctx.Int64("sat_per_byte"),
		MinHtlcMsat:      ctx.Int64("min_htlc_msat"),
		RemoteCsvDelay:   uint32(ctx.Uint64("remote_csv_delay")),
		MinConfs:         minConfs,
		SpendUnconfirmed: minConfs == 0,
		CloseAddress:     ctx.String("close_address"),
	}

	switch {
	case ctx.IsSet("node_key"):
		nodePubHex, err := hex.DecodeString(ctx.String("node_key"))
		if err != nil {
			return fmt.Errorf("unable to decode node public key: %v", err)
		}
		req.NodePubkey = nodePubHex

	case args.Present():
		nodePubHex, err := hex.DecodeString(args.First())
		if err != nil {
			return fmt.Errorf("unable to decode node public key: %v", err)
		}
		args = args.Tail()
		req.NodePubkey = nodePubHex
	default:
		return fmt.Errorf("node id argument missing")
	}

	// As soon as we can confirm that the node's node_key was set, rather
	// than the peer_id, we can check if the host:port was also set to
	// connect to it before opening the channel.
	if req.NodePubkey != nil && ctx.IsSet("connect") {
		addr := &lnrpc.LightningAddress{
			Pubkey: hex.EncodeToString(req.NodePubkey),
			Host:   ctx.String("connect"),
		}

		req := &lnrpc.ConnectPeerRequest{
			Addr: addr,
			Perm: false,
		}

		// Check if connecting to the node was successful.
		// We discard the peer id returned as it is not needed.
		_, err := client.ConnectPeer(ctxb, req)
		if err != nil &&
			!strings.Contains(err.Error(), "already connected") {
			return err
		}
	}

	switch {
	case ctx.IsSet("local_amt"):
		req.LocalFundingAmount = int64(ctx.Int("local_amt"))
	case args.Present():
		req.LocalFundingAmount, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode local amt: %v", err)
		}
		args = args.Tail()
	default:
		return fmt.Errorf("local amt argument missing")
	}

	if ctx.IsSet("push_amt") {
		req.PushSat = int64(ctx.Int("push_amt"))
	} else if args.Present() {
		req.PushSat, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode push amt: %v", err)
		}
	}

	req.Private = ctx.Bool("private")

	stream, err := client.OpenChannel(ctxb, req)
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		switch update := resp.Update.(type) {
		case *lnrpc.OpenStatusUpdate_ChanPending:
			txid, err := chainhash.NewHash(update.ChanPending.Txid)
			if err != nil {
				return err
			}

			printJSON(struct {
				FundingTxid string `json:"funding_txid"`
			}{
				FundingTxid: txid.String(),
			},
			)

			if !ctx.Bool("block") {
				return nil
			}

		case *lnrpc.OpenStatusUpdate_ChanOpen:
			channelPoint := update.ChanOpen.ChannelPoint

			// A channel point's funding txid can be get/set as a
			// byte slice or a string. In the case it is a string,
			// decode it.
			var txidHash []byte
			switch channelPoint.GetFundingTxid().(type) {
			case *lnrpc.ChannelPoint_FundingTxidBytes:
				txidHash = channelPoint.GetFundingTxidBytes()
			case *lnrpc.ChannelPoint_FundingTxidStr:
				s := channelPoint.GetFundingTxidStr()
				h, err := chainhash.NewHashFromStr(s)
				if err != nil {
					return err
				}

				txidHash = h[:]
			}

			txid, err := chainhash.NewHash(txidHash)
			if err != nil {
				return err
			}

			index := channelPoint.OutputIndex
			printJSON(struct {
				ChannelPoint string `json:"channel_point"`
			}{
				ChannelPoint: fmt.Sprintf("%v:%v", txid, index),
			},
			)
		}
	}
}
