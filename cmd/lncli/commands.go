package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/urfave/cli"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TODO(roasbeef): cli logic for supporting both positional and unix style
// arguments.

// TODO(roasbeef): expose all fee conf targets

const defaultRecoveryWindow int32 = 2500

func printJSON(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	json.Indent(&out, b, "", "\t")
	out.WriteString("\n")
	out.WriteTo(os.Stdout)
}

func printRespJSON(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
		EmitDefaults: true,
		Indent:       "    ",
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

// actionDecorator is used to add additional information and error handling
// to command actions.
func actionDecorator(f func(*cli.Context) error) func(*cli.Context) error {
	return func(c *cli.Context) error {
		if err := f(c); err != nil {
			s, ok := status.FromError(err)

			// If it's a command for the UnlockerService (like
			// 'create' or 'unlock') but the wallet is already
			// unlocked, then these methods aren't recognized any
			// more because this service is shut down after
			// successful unlock. That's why the code
			// 'Unimplemented' means something different for these
			// two commands.
			if s.Code() == codes.Unimplemented &&
				(c.Command.Name == "create" ||
					c.Command.Name == "unlock") {
				return fmt.Errorf("Wallet is already unlocked")
			}

			// lnd might be active, but not possible to contact
			// using RPC if the wallet is encrypted. If we get
			// error code Unimplemented, it means that lnd is
			// running, but the RPC server is not active yet (only
			// WalletUnlocker server active) and most likely this
			// is because of an encrypted wallet.
			if ok && s.Code() == codes.Unimplemented {
				return fmt.Errorf("Wallet is encrypted. " +
					"Please unlock using 'lncli unlock', " +
					"or set password using 'lncli create'" +
					" if this is the first time starting " +
					"lnd.")
			}
			return err
		}
		return nil
	}
}

var newAddressCommand = cli.Command{
	Name:      "newaddress",
	Category:  "Wallet",
	Usage:     "Generates a new address.",
	ArgsUsage: "address-type",
	Description: `
	Generate a wallet new address. Address-types has to be one of:
	    - p2wkh:  Pay to witness key hash
	    - np2wkh: Pay to nested witness key hash`,
	Action: actionDecorator(newAddress),
}

func newAddress(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	stringAddrType := ctx.Args().First()

	// Map the string encoded address type, to the concrete typed address
	// type enum. An unrecognized address type will result in an error.
	var addrType lnrpc.AddressType
	switch stringAddrType { // TODO(roasbeef): make them ints on the cli?
	case "p2wkh":
		addrType = lnrpc.AddressType_WITNESS_PUBKEY_HASH
	case "np2wkh":
		addrType = lnrpc.AddressType_NESTED_PUBKEY_HASH
	default:
		return fmt.Errorf("invalid address type %v, support address type "+
			"are: p2wkh and np2wkh", stringAddrType)
	}

	ctxb := context.Background()
	addr, err := client.NewAddress(ctxb, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	if err != nil {
		return err
	}

	printRespJSON(addr)
	return nil
}

var estimateFeeCommand = cli.Command{
	Name:      "estimatefee",
	Category:  "On-chain",
	Usage:     "Get fee estimates for sending bitcoin on-chain to multiple addresses.",
	ArgsUsage: "send-json-string [--conf_target=N]",
	Description: `
	Get fee estimates for sending a transaction paying the specified amount(s) to the passed address(es).

	The send-json-string' param decodes addresses and the amount to send respectively in the following format:

	    '{"ExampleAddr": NumCoinsInSatoshis, "SecondAddr": NumCoins}'
	`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "conf_target",
			Usage: "(optional) the number of blocks that the transaction *should* " +
				"confirm in",
		},
	},
	Action: actionDecorator(estimateFees),
}

func estimateFees(ctx *cli.Context) error {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().First()
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	resp, err := client.EstimateFee(ctxb, &lnrpc.EstimateFeeRequest{
		AddrToAmount: amountToAddr,
		TargetConf:   int32(ctx.Int64("conf_target")),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var sendCoinsCommand = cli.Command{
	Name:      "sendcoins",
	Category:  "On-chain",
	Usage:     "Send bitcoin on-chain to an address.",
	ArgsUsage: "addr amt",
	Description: `
	Send amt coins in satoshis to the BASE58 encoded bitcoin address addr.

	Fees used when sending the transaction can be specified via the --conf_target, or
	--sat_per_byte optional flags.

	Positional arguments and flags can be used interchangeably but not at the same time!
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "addr",
			Usage: "the BASE58 encoded bitcoin address to send coins to on-chain",
		},
		cli.BoolFlag{
			Name: "sweepall",
			Usage: "if set, then the amount field will be ignored, " +
				"and all the wallet will attempt to sweep all " +
				"outputs within the wallet to the target " +
				"address",
		},
		cli.Int64Flag{
			Name:  "amt",
			Usage: "the number of bitcoin denominated in satoshis to send",
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
	},
	Action: actionDecorator(sendCoins),
}

func sendCoins(ctx *cli.Context) error {
	var (
		addr string
		amt  int64
		err  error
	)
	args := ctx.Args()

	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendcoins")
		return nil
	}

	if ctx.IsSet("conf_target") && ctx.IsSet("sat_per_byte") {
		return fmt.Errorf("either conf_target or sat_per_byte should be " +
			"set, but not both")
	}

	switch {
	case ctx.IsSet("addr"):
		addr = ctx.String("addr")
	case args.Present():
		addr = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("Address argument missing")
	}

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
	case !ctx.Bool("sweepall"):
		return fmt.Errorf("Amount argument missing")
	}
	if err != nil {
		return fmt.Errorf("unable to decode amount: %v", err)
	}

	if amt != 0 && ctx.Bool("sweepall") {
		return fmt.Errorf("amount cannot be set if attempting to " +
			"sweep all coins out of the wallet")
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.SendCoinsRequest{
		Addr:       addr,
		Amount:     amt,
		TargetConf: int32(ctx.Int64("conf_target")),
		SatPerByte: ctx.Int64("sat_per_byte"),
		SendAll:    ctx.Bool("sweepall"),
	}
	txid, err := client.SendCoins(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(txid)
	return nil
}

var listUnspentCommand = cli.Command{
	Name:      "listunspent",
	Category:  "On-chain",
	Usage:     "List utxos available for spending.",
	ArgsUsage: "[min-confs [max-confs]] [--unconfirmed_only]",
	Description: `
	For each spendable utxo currently in the wallet, with at least min_confs
	confirmations, and at most max_confs confirmations, lists the txid,
	index, amount, address, address type, scriptPubkey and number of
	confirmations.  Use --min_confs=0 to include unconfirmed coins. To list
	all coins with at least min_confs confirmations, omit the second
	argument or flag '--max_confs'. To list all confirmed and unconfirmed
	coins, no arguments are required. To see only unconfirmed coins, use
	'--unconfirmed_only' with '--min_confs' and '--max_confs' set to zero or
	not present.
	`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "min_confs",
			Usage: "the minimum number of confirmations for a utxo",
		},
		cli.Int64Flag{
			Name:  "max_confs",
			Usage: "the maximum number of confirmations for a utxo",
		},
		cli.BoolFlag{
			Name: "unconfirmed_only",
			Usage: "when min_confs and max_confs are zero, " +
				"setting false implicitly overrides max_confs " +
				"to be MaxInt32, otherwise max_confs remains " +
				"zero. An error is returned if the value is " +
				"true and both min_confs and max_confs are " +
				"non-zero. (default: false)",
		},
	},
	Action: actionDecorator(listUnspent),
}

func listUnspent(ctx *cli.Context) error {
	var (
		minConfirms int64
		maxConfirms int64
		err         error
	)
	args := ctx.Args()

	if ctx.IsSet("max_confs") && !ctx.IsSet("min_confs") {
		return fmt.Errorf("max_confs cannot be set without " +
			"min_confs being set")
	}

	switch {
	case ctx.IsSet("min_confs"):
		minConfirms = ctx.Int64("min_confs")
	case args.Present():
		minConfirms, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			cli.ShowCommandHelp(ctx, "listunspent")
			return nil
		}
		args = args.Tail()
	}

	switch {
	case ctx.IsSet("max_confs"):
		maxConfirms = ctx.Int64("max_confs")
	case args.Present():
		maxConfirms, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			cli.ShowCommandHelp(ctx, "listunspent")
			return nil
		}
		args = args.Tail()
	}

	unconfirmedOnly := ctx.Bool("unconfirmed_only")

	// Force minConfirms and maxConfirms to be zero if unconfirmedOnly is
	// true.
	if unconfirmedOnly && (minConfirms != 0 || maxConfirms != 0) {
		cli.ShowCommandHelp(ctx, "listunspent")
		return nil
	}

	// When unconfirmedOnly is inactive, we will override maxConfirms to be
	// a MaxInt32 to return all confirmed and unconfirmed utxos.
	if maxConfirms == 0 && !unconfirmedOnly {
		maxConfirms = math.MaxInt32
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListUnspentRequest{
		MinConfs: int32(minConfirms),
		MaxConfs: int32(maxConfirms),
	}
	resp, err := client.ListUnspent(ctxb, req)
	if err != nil {
		return err
	}

	// Parse the response into the final json object that will be printed
	// to stdout. At the moment, this filters out the raw txid bytes from
	// each utxo's outpoint and only prints the txid string.
	var listUnspentResp = struct {
		Utxos []*Utxo `json:"utxos"`
	}{
		Utxos: make([]*Utxo, 0, len(resp.Utxos)),
	}
	for _, protoUtxo := range resp.Utxos {
		utxo := NewUtxoFromProto(protoUtxo)
		listUnspentResp.Utxos = append(listUnspentResp.Utxos, utxo)
	}

	printJSON(listUnspentResp)

	return nil
}

var sendManyCommand = cli.Command{
	Name:      "sendmany",
	Category:  "On-chain",
	Usage:     "Send bitcoin on-chain to multiple addresses.",
	ArgsUsage: "send-json-string [--conf_target=N] [--sat_per_byte=P]",
	Description: `
	Create and broadcast a transaction paying the specified amount(s) to the passed address(es).

	The send-json-string' param decodes addresses and the amount to send
	respectively in the following format:

	    '{"ExampleAddr": NumCoinsInSatoshis, "SecondAddr": NumCoins}'
	`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "conf_target",
			Usage: "(optional) the number of blocks that the transaction *should* " +
				"confirm in, will be used for fee estimation",
		},
		cli.Int64Flag{
			Name: "sat_per_byte",
			Usage: "(optional) a manual fee expressed in sat/byte that should be " +
				"used when crafting the transaction",
		},
	},
	Action: actionDecorator(sendMany),
}

func sendMany(ctx *cli.Context) error {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().First()
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	if ctx.IsSet("conf_target") && ctx.IsSet("sat_per_byte") {
		return fmt.Errorf("either conf_target or sat_per_byte should be " +
			"set, but not both")
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{
		AddrToAmount: amountToAddr,
		TargetConf:   int32(ctx.Int64("conf_target")),
		SatPerByte:   ctx.Int64("sat_per_byte"),
	})
	if err != nil {
		return err
	}

	printRespJSON(txid)
	return nil
}

var connectCommand = cli.Command{
	Name:      "connect",
	Category:  "Peers",
	Usage:     "Connect to a remote lnd peer.",
	ArgsUsage: "<pubkey>@host",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "perm",
			Usage: "If set, the daemon will attempt to persistently " +
				"connect to the target peer.\n" +
				"           If not, the call will be synchronous.",
		},
	},
	Action: actionDecorator(connectPeer),
}

func connectPeer(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	targetAddress := ctx.Args().First()
	splitAddr := strings.Split(targetAddress, "@")
	if len(splitAddr) != 2 {
		return fmt.Errorf("target address expected in format: " +
			"pubkey@host:port")
	}

	addr := &lnrpc.LightningAddress{
		Pubkey: splitAddr[0],
		Host:   splitAddr[1],
	}
	req := &lnrpc.ConnectPeerRequest{
		Addr: addr,
		Perm: ctx.Bool("perm"),
	}

	lnid, err := client.ConnectPeer(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(lnid)
	return nil
}

var disconnectCommand = cli.Command{
	Name:      "disconnect",
	Category:  "Peers",
	Usage:     "Disconnect a remote lnd peer identified by public key.",
	ArgsUsage: "<pubkey>",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "node_key",
			Usage: "The hex-encoded compressed public key of the peer " +
				"to disconnect from",
		},
	},
	Action: actionDecorator(disconnectPeer),
}

func disconnectPeer(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var pubKey string
	switch {
	case ctx.IsSet("node_key"):
		pubKey = ctx.String("node_key")
	case ctx.Args().Present():
		pubKey = ctx.Args().First()
	default:
		return fmt.Errorf("must specify target public key")
	}

	req := &lnrpc.DisconnectPeerRequest{
		PubKey: pubKey,
	}

	lnid, err := client.DisconnectPeer(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(lnid)
	return nil
}

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

// TODO(roasbeef): also allow short relative channel ID.

var closeChannelCommand = cli.Command{
	Name:     "closechannel",
	Category: "Channels",
	Usage:    "Close an existing channel.",
	Description: `
	Close an existing channel. The channel can be closed either cooperatively,
	or unilaterally (--force).

	A unilateral channel closure means that the latest commitment
	transaction will be broadcast to the network. As a result, any settled
	funds will be time locked for a few blocks before they can be spent.

	In the case of a cooperative closure, One can manually set the fee to
	be used for the closing transaction via either the --conf_target or
	--sat_per_byte arguments. This will be the starting value used during
	fee negotiation. This is optional.

	To view which funding_txids/output_indexes can be used for a channel close,
	see the channel_point values within the listchannels command output.
	The format for a channel_point is 'funding_txid:output_index'.`,
	ArgsUsage: "funding_txid [output_index]",
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
		cli.BoolFlag{
			Name:  "force",
			Usage: "attempt an uncooperative closure",
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block until the channel is closed",
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
	},
	Action: actionDecorator(closeChannel),
}

func closeChannel(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments and flags were provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "closechannel")
		return nil
	}

	channelPoint, err := parseChannelPoint(ctx)
	if err != nil {
		return err
	}

	// TODO(roasbeef): implement time deadline within server
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: channelPoint,
		Force:        ctx.Bool("force"),
		TargetConf:   int32(ctx.Int64("conf_target")),
		SatPerByte:   ctx.Int64("sat_per_byte"),
	}

	// After parsing the request, we'll spin up a goroutine that will
	// retrieve the closing transaction ID when attempting to close the
	// channel. We do this to because `executeChannelClose` can block, so we
	// would like to present the closing transaction ID to the user as soon
	// as it is broadcasted.
	var wg sync.WaitGroup
	txidChan := make(chan string, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()

		printJSON(struct {
			ClosingTxid string `json:"closing_txid"`
		}{
			ClosingTxid: <-txidChan,
		})
	}()

	err = executeChannelClose(client, req, txidChan, ctx.Bool("block"))
	if err != nil {
		return err
	}

	// In the case that the user did not provide the `block` flag, then we
	// need to wait for the goroutine to be done to prevent it from being
	// destroyed when exiting before printing the closing transaction ID.
	wg.Wait()

	return nil
}

// executeChannelClose attempts to close the channel from a request. The closing
// transaction ID is sent through `txidChan` as soon as it is broadcasted to the
// network. The block boolean is used to determine if we should block until the
// closing transaction receives all of its required confirmations.
func executeChannelClose(client lnrpc.LightningClient, req *lnrpc.CloseChannelRequest,
	txidChan chan<- string, block bool) error {

	stream, err := client.CloseChannel(context.Background(), req)
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
		case *lnrpc.CloseStatusUpdate_ClosePending:
			closingHash := update.ClosePending.Txid
			txid, err := chainhash.NewHash(closingHash)
			if err != nil {
				return err
			}

			txidChan <- txid.String()

			if !block {
				return nil
			}
		case *lnrpc.CloseStatusUpdate_ChanClose:
			return nil
		}
	}
}

var closeAllChannelsCommand = cli.Command{
	Name:     "closeallchannels",
	Category: "Channels",
	Usage:    "Close all existing channels.",
	Description: `
	Close all existing channels.

	Channels will be closed either cooperatively or unilaterally, depending
	on whether the channel is active or not. If the channel is inactive, any
	settled funds within it will be time locked for a few blocks before they
	can be spent.

	One can request to close inactive channels only by using the
	--inactive_only flag.

	By default, one is prompted for confirmation every time an inactive
	channel is requested to be closed. To avoid this, one can set the
	--force flag, which will only prompt for confirmation once for all
	inactive channels and proceed to close them.

	In the case of cooperative closures, one can manually set the fee to
	be used for the closing transactions via either the --conf_target or
	--sat_per_byte arguments. This will be the starting value used during
	fee negotiation. This is optional.`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "inactive_only",
			Usage: "close inactive channels only",
		},
		cli.BoolFlag{
			Name: "force",
			Usage: "ask for confirmation once before attempting " +
				"to close existing channels",
		},
		cli.Int64Flag{
			Name: "conf_target",
			Usage: "(optional) the number of blocks that the " +
				"closing transactions *should* confirm in, will be " +
				"used for fee estimation",
		},
		cli.Int64Flag{
			Name: "sat_per_byte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/byte that should be used when crafting " +
				"the closing transactions",
		},
	},
	Action: actionDecorator(closeAllChannels),
}

func closeAllChannels(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	listReq := &lnrpc.ListChannelsRequest{}
	openChannels, err := client.ListChannels(context.Background(), listReq)
	if err != nil {
		return fmt.Errorf("unable to fetch open channels: %v", err)
	}

	if len(openChannels.Channels) == 0 {
		return errors.New("no open channels to close")
	}

	var channelsToClose []*lnrpc.Channel

	switch {
	case ctx.Bool("force") && ctx.Bool("inactive_only"):
		msg := "Unilaterally close all inactive channels? The funds " +
			"within these channels will be locked for some blocks " +
			"(CSV delay) before they can be spent. (yes/no): "

		confirmed := promptForConfirmation(msg)

		// We can safely exit if the user did not confirm.
		if !confirmed {
			return nil
		}

		// Go through the list of open channels and only add inactive
		// channels to the closing list.
		for _, channel := range openChannels.Channels {
			if !channel.GetActive() {
				channelsToClose = append(
					channelsToClose, channel,
				)
			}
		}
	case ctx.Bool("force"):
		msg := "Close all active and inactive channels? Inactive " +
			"channels will be closed unilaterally, so funds " +
			"within them will be locked for a few blocks (CSV " +
			"delay) before they can be spent. (yes/no): "

		confirmed := promptForConfirmation(msg)

		// We can safely exit if the user did not confirm.
		if !confirmed {
			return nil
		}

		channelsToClose = openChannels.Channels
	default:
		// Go through the list of open channels and determine which
		// should be added to the closing list.
		for _, channel := range openChannels.Channels {
			// If the channel is inactive, we'll attempt to
			// unilaterally close the channel, so we should prompt
			// the user for confirmation beforehand.
			if !channel.GetActive() {
				msg := fmt.Sprintf("Unilaterally close channel "+
					"with node %s and channel point %s? "+
					"The closing transaction will need %d "+
					"confirmations before the funds can be "+
					"spent. (yes/no): ", channel.RemotePubkey,
					channel.ChannelPoint, channel.CsvDelay)

				confirmed := promptForConfirmation(msg)

				if confirmed {
					channelsToClose = append(
						channelsToClose, channel,
					)
				}
			} else if !ctx.Bool("inactive_only") {
				// Otherwise, we'll only add active channels if
				// we were not requested to close inactive
				// channels only.
				channelsToClose = append(
					channelsToClose, channel,
				)
			}
		}
	}

	// result defines the result of closing a channel. The closing
	// transaction ID is populated if a channel is successfully closed.
	// Otherwise, the error that prevented closing the channel is populated.
	type result struct {
		RemotePubKey string `json:"remote_pub_key"`
		ChannelPoint string `json:"channel_point"`
		ClosingTxid  string `json:"closing_txid"`
		FailErr      string `json:"error"`
	}

	// Launch each channel closure in a goroutine in order to execute them
	// in parallel. Once they're all executed, we will print the results as
	// they come.
	resultChan := make(chan result, len(channelsToClose))
	for _, channel := range channelsToClose {
		go func(channel *lnrpc.Channel) {
			res := result{}
			res.RemotePubKey = channel.RemotePubkey
			res.ChannelPoint = channel.ChannelPoint
			defer func() {
				resultChan <- res
			}()

			// Parse the channel point in order to create the close
			// channel request.
			s := strings.Split(res.ChannelPoint, ":")
			if len(s) != 2 {
				res.FailErr = "expected channel point with " +
					"format txid:index"
				return
			}
			index, err := strconv.ParseUint(s[1], 10, 32)
			if err != nil {
				res.FailErr = fmt.Sprintf("unable to parse "+
					"channel point output index: %v", err)
				return
			}

			req := &lnrpc.CloseChannelRequest{
				ChannelPoint: &lnrpc.ChannelPoint{
					FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
						FundingTxidStr: s[0],
					},
					OutputIndex: uint32(index),
				},
				Force:      !channel.GetActive(),
				TargetConf: int32(ctx.Int64("conf_target")),
				SatPerByte: ctx.Int64("sat_per_byte"),
			}

			txidChan := make(chan string, 1)
			err = executeChannelClose(client, req, txidChan, false)
			if err != nil {
				res.FailErr = fmt.Sprintf("unable to close "+
					"channel: %v", err)
				return
			}

			res.ClosingTxid = <-txidChan
		}(channel)
	}

	for range channelsToClose {
		res := <-resultChan
		printJSON(res)
	}

	return nil
}

// promptForConfirmation continuously prompts the user for the message until
// receiving a response of "yes" or "no" and returns their answer as a bool.
func promptForConfirmation(msg string) bool {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print(msg)

		answer, err := reader.ReadString('\n')
		if err != nil {
			return false
		}

		answer = strings.ToLower(strings.TrimSpace(answer))

		switch {
		case answer == "yes":
			return true
		case answer == "no":
			return false
		default:
			continue
		}
	}
}

var abandonChannelCommand = cli.Command{
	Name:     "abandonchannel",
	Category: "Channels",
	Usage:    "Abandons an existing channel.",
	Description: `
	Removes all channel state from the database except for a close
	summary. This method can be used to get rid of permanently unusable
	channels due to bugs fixed in newer versions of lnd.

	Only available when lnd is built in debug mode.

	To view which funding_txids/output_indexes can be used for this command,
	see the channel_point values within the listchannels command output.
	The format for a channel_point is 'funding_txid:output_index'.`,
	ArgsUsage: "funding_txid [output_index]",
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
	},
	Action: actionDecorator(abandonChannel),
}

func abandonChannel(ctx *cli.Context) error {
	ctxb := context.Background()

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments and flags were provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "abandonchannel")
		return nil
	}

	channelPoint, err := parseChannelPoint(ctx)
	if err != nil {
		return err
	}

	req := &lnrpc.AbandonChannelRequest{
		ChannelPoint: channelPoint,
	}

	resp, err := client.AbandonChannel(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

// parseChannelPoint parses a funding txid and output index from the command
// line. Both named options as well as unnamed parameters are supported.
func parseChannelPoint(ctx *cli.Context) (*lnrpc.ChannelPoint, error) {
	channelPoint := &lnrpc.ChannelPoint{}

	args := ctx.Args()

	switch {
	case ctx.IsSet("funding_txid"):
		channelPoint.FundingTxid = &lnrpc.ChannelPoint_FundingTxidStr{
			FundingTxidStr: ctx.String("funding_txid"),
		}
	case args.Present():
		channelPoint.FundingTxid = &lnrpc.ChannelPoint_FundingTxidStr{
			FundingTxidStr: args.First(),
		}
		args = args.Tail()
	default:
		return nil, fmt.Errorf("funding txid argument missing")
	}

	switch {
	case ctx.IsSet("output_index"):
		channelPoint.OutputIndex = uint32(ctx.Int("output_index"))
	case args.Present():
		index, err := strconv.ParseUint(args.First(), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("unable to decode output index: %v", err)
		}
		channelPoint.OutputIndex = uint32(index)
	default:
		channelPoint.OutputIndex = 0
	}

	return channelPoint, nil
}

var listPeersCommand = cli.Command{
	Name:     "listpeers",
	Category: "Peers",
	Usage:    "List all active, currently connected peers.",
	Action:   actionDecorator(listPeers),
}

func listPeers(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPeersRequest{}
	resp, err := client.ListPeers(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var createCommand = cli.Command{
	Name:     "create",
	Category: "Startup",
	Usage:    "Initialize a wallet when starting lnd for the first time.",
	Description: `
	The create command is used to initialize an lnd wallet from scratch for
	the very first time. This is interactive command with one required
	argument (the password), and one optional argument (the mnemonic
	passphrase).

	The first argument (the password) is required and MUST be greater than
	8 characters. This will be used to encrypt the wallet within lnd. This
	MUST be remembered as it will be required to fully start up the daemon.

	The second argument is an optional 24-word mnemonic derived from BIP
	39. If provided, then the internal wallet will use the seed derived
	from this mnemonic to generate all keys.

	This command returns a 24-word seed in the scenario that NO mnemonic
	was provided by the user. This should be written down as it can be used
	to potentially recover all on-chain funds, and most off-chain funds as
	well.

	Finally, it's also possible to use this command and a set of static
	channel backups to trigger a recover attempt for the provided Static
	Channel Backups. Only one of the three parameters will be accepted. See
	the restorechanbackup command for further details w.r.t the format
	accepted.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "single_backup",
			Usage: "a hex encoded single channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name: "multi_backup",
			Usage: "a hex encoded multi-channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name:  "multi_file",
			Usage: "the path to a multi-channel back up file",
		},
	},
	Action: actionDecorator(create),
}

// monowidthColumns takes a set of words, and the number of desired columns,
// and returns a new set of words that have had white space appended to the
// word in order to create a mono-width column.
func monowidthColumns(words []string, ncols int) []string {
	// Determine max size of words in each column.
	colWidths := make([]int, ncols)
	for i, word := range words {
		col := i % ncols
		curWidth := colWidths[col]
		if len(word) > curWidth {
			colWidths[col] = len(word)
		}
	}

	// Append whitespace to each word to make columns mono-width.
	finalWords := make([]string, len(words))
	for i, word := range words {
		col := i % ncols
		width := colWidths[col]

		diff := width - len(word)
		finalWords[i] = word + strings.Repeat(" ", diff)
	}

	return finalWords
}

func create(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getWalletUnlockerClient(ctx)
	defer cleanUp()

	walletPassword, err := capturePassword(
		"Input wallet password: ", false, walletunlocker.ValidatePassword,
	)
	if err != nil {
		return err
	}

	// Next, we'll see if the user has 24-word mnemonic they want to use to
	// derive a seed within the wallet.
	var (
		hasMnemonic bool
	)

mnemonicCheck:
	for {
		fmt.Println()
		fmt.Printf("Do you have an existing cipher seed " +
			"mnemonic you want to use? (Enter y/n): ")

		reader := bufio.NewReader(os.Stdin)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		fmt.Println()

		answer = strings.TrimSpace(answer)
		answer = strings.ToLower(answer)

		switch answer {
		case "y":
			hasMnemonic = true
			break mnemonicCheck
		case "n":
			hasMnemonic = false
			break mnemonicCheck
		}
	}

	// If the user *does* have an existing seed they want to use, then
	// we'll read that in directly from the terminal.
	var (
		cipherSeedMnemonic []string
		aezeedPass         []byte
		recoveryWindow     int32
	)
	if hasMnemonic {
		// We'll now prompt the user to enter in their 24-word
		// mnemonic.
		fmt.Printf("Input your 24-word mnemonic separated by spaces: ")
		reader := bufio.NewReader(os.Stdin)
		mnemonic, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		// We'll trim off extra spaces, and ensure the mnemonic is all
		// lower case, then populate our request.
		mnemonic = strings.TrimSpace(mnemonic)
		mnemonic = strings.ToLower(mnemonic)

		cipherSeedMnemonic = strings.Split(mnemonic, " ")

		fmt.Println()

		if len(cipherSeedMnemonic) != 24 {
			return fmt.Errorf("wrong cipher seed mnemonic "+
				"length: got %v words, expecting %v words",
				len(cipherSeedMnemonic), 24)
		}

		// Additionally, the user may have a passphrase, that will also
		// need to be provided so the daemon can properly decipher the
		// cipher seed.
		fmt.Printf("Input your cipher seed passphrase (press enter if " +
			"your seed doesn't have a passphrase): ")
		passphrase, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			return err
		}

		aezeedPass = []byte(passphrase)

		for {
			fmt.Println()
			fmt.Printf("Input an optional address look-ahead "+
				"used to scan for used keys (default %d): ",
				defaultRecoveryWindow)

			reader := bufio.NewReader(os.Stdin)
			answer, err := reader.ReadString('\n')
			if err != nil {
				return err
			}

			fmt.Println()

			answer = strings.TrimSpace(answer)

			if len(answer) == 0 {
				recoveryWindow = defaultRecoveryWindow
				break
			}

			lookAhead, err := strconv.Atoi(answer)
			if err != nil {
				fmt.Println("Unable to parse recovery "+
					"window: %v", err)
				continue
			}

			recoveryWindow = int32(lookAhead)
			break
		}
	} else {
		// Otherwise, if the user doesn't have a mnemonic that they
		// want to use, we'll generate a fresh one with the GenSeed
		// command.
		fmt.Println("Your cipher seed can optionally be encrypted.")

		instruction := "Input your passphrase if you wish to encrypt it " +
			"(or press enter to proceed without a cipher seed " +
			"passphrase): "
		aezeedPass, err = capturePassword(
			instruction, true, func(_ []byte) error { return nil },
		)
		if err != nil {
			return err
		}

		fmt.Println()
		fmt.Println("Generating fresh cipher seed...")
		fmt.Println()

		genSeedReq := &lnrpc.GenSeedRequest{
			AezeedPassphrase: aezeedPass,
		}
		seedResp, err := client.GenSeed(ctxb, genSeedReq)
		if err != nil {
			return fmt.Errorf("unable to generate seed: %v", err)
		}

		cipherSeedMnemonic = seedResp.CipherSeedMnemonic
	}

	// Before we initialize the wallet, we'll display the cipher seed to
	// the user so they can write it down.
	mnemonicWords := cipherSeedMnemonic

	fmt.Println("!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO " +
		"RESTORE THE WALLET!!!\n")

	fmt.Println("---------------BEGIN LND CIPHER SEED---------------")

	numCols := 4
	colWords := monowidthColumns(mnemonicWords, numCols)
	for i := 0; i < len(colWords); i += numCols {
		fmt.Printf("%2d. %3s  %2d. %3s  %2d. %3s  %2d. %3s\n",
			i+1, colWords[i], i+2, colWords[i+1], i+3,
			colWords[i+2], i+4, colWords[i+3])
	}

	fmt.Println("---------------END LND CIPHER SEED-----------------")

	fmt.Println("\n!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO " +
		"RESTORE THE WALLET!!!")

	// We'll also check to see if they provided any static channel backups,
	// if so, then we'll also tack these onto the final innit wallet
	// request.
	backups, err := parseChanBackups(ctx)
	if err != nil {
		return fmt.Errorf("unable to parse chan "+
			"backups: %v", err)
	}

	var chanBackups *lnrpc.ChanBackupSnapshot
	if backups != nil {
		switch {
		case backups.GetChanBackups() != nil:
			singleBackup := backups.GetChanBackups()
			chanBackups = &lnrpc.ChanBackupSnapshot{
				SingleChanBackups: singleBackup,
			}

		case backups.GetMultiChanBackup() != nil:
			multiBackup := backups.GetMultiChanBackup()
			chanBackups = &lnrpc.ChanBackupSnapshot{
				MultiChanBackup: &lnrpc.MultiChanBackup{
					MultiChanBackup: multiBackup,
				},
			}
		}
	}

	// With either the user's prior cipher seed, or a newly generated one,
	// we'll go ahead and initialize the wallet.
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     walletPassword,
		CipherSeedMnemonic: cipherSeedMnemonic,
		AezeedPassphrase:   aezeedPass,
		RecoveryWindow:     recoveryWindow,
		ChannelBackups:     chanBackups,
	}
	if _, err := client.InitWallet(ctxb, req); err != nil {
		return err
	}

	fmt.Println("\nlnd successfully initialized!")

	return nil
}

// capturePassword returns a password value that has been entered twice by the
// user, to ensure that the user knows what password they have entered. The user
// will be prompted to retry until the passwords match. If the optional param is
// true, the function may return an empty byte array if the user opts against
// using a password.
func capturePassword(instruction string, optional bool,
	validate func([]byte) error) ([]byte, error) {

	for {
		fmt.Printf(instruction)
		password, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			return nil, err
		}
		fmt.Println()

		// Do not require users to repeat password if
		// it is optional and they are not using one.
		if len(password) == 0 && optional {
			return nil, nil
		}

		// If the password provided is not valid, restart
		// password capture process from the beginning.
		if err := validate(password); err != nil {
			fmt.Println(err.Error())
			fmt.Println()
			continue
		}

		fmt.Println("Confirm password:")
		passwordConfirmed, err := terminal.ReadPassword(
			int(syscall.Stdin),
		)
		if err != nil {
			return nil, err
		}
		fmt.Println()

		if bytes.Equal(password, passwordConfirmed) {
			return password, nil
		}

		fmt.Println("Passwords don't match, " +
			"please try again")
		fmt.Println()
	}
}

var unlockCommand = cli.Command{
	Name:     "unlock",
	Category: "Startup",
	Usage:    "Unlock an encrypted wallet at startup.",
	Description: `
	The unlock command is used to decrypt lnd's wallet state in order to
	start up. This command MUST be run after booting up lnd before it's
	able to carry out its duties. An exception is if a user is running with
	--noseedbackup, then a default passphrase will be used.
	`,
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "recovery_window",
			Usage: "address lookahead to resume recovery rescan, " +
				"value should be non-zero --  To recover all " +
				"funds, this should be greater than the " +
				"maximum number of consecutive, unused " +
				"addresses ever generated by the wallet.",
		},
	},
	Action: actionDecorator(unlock),
}

func unlock(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getWalletUnlockerClient(ctx)
	defer cleanUp()

	fmt.Printf("Input wallet password: ")
	pw, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return err
	}
	fmt.Println()

	args := ctx.Args()

	// Parse the optional recovery window if it is specified. By default,
	// the recovery window will be 0, indicating no lookahead should be
	// used.
	var recoveryWindow int32
	switch {
	case ctx.IsSet("recovery_window"):
		recoveryWindow = int32(ctx.Int64("recovery_window"))
	case args.Present():
		window, err := strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return err
		}
		recoveryWindow = int32(window)
	}

	req := &lnrpc.UnlockWalletRequest{
		WalletPassword: pw,
		RecoveryWindow: recoveryWindow,
	}
	_, err = client.UnlockWallet(ctxb, req)
	if err != nil {
		return err
	}

	fmt.Println("\nlnd successfully unlocked!")

	// TODO(roasbeef): add ability to accept hex single and multi backups

	return nil
}

var changePasswordCommand = cli.Command{
	Name:     "changepassword",
	Category: "Startup",
	Usage:    "Change an encrypted wallet's password at startup.",
	Description: `
	The changepassword command is used to Change lnd's encrypted wallet's
	password. It will automatically unlock the daemon if the password change
	is successful.

	If one did not specify a password for their wallet (running lnd with
	--noseedbackup), one must restart their daemon without
	--noseedbackup and use this command. The "current password" field
	should be left empty.
	`,
	Action: actionDecorator(changePassword),
}

func changePassword(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getWalletUnlockerClient(ctx)
	defer cleanUp()

	fmt.Printf("Input current wallet password: ")
	currentPw, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return err
	}
	fmt.Println()

	fmt.Printf("Input new wallet password: ")
	newPw, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return err
	}
	fmt.Println()

	fmt.Printf("Confirm new wallet password: ")
	confirmPw, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return err
	}
	fmt.Println()

	if !bytes.Equal(newPw, confirmPw) {
		return fmt.Errorf("passwords don't match")
	}

	req := &lnrpc.ChangePasswordRequest{
		CurrentPassword: currentPw,
		NewPassword:     newPw,
	}

	_, err = client.ChangePassword(ctxb, req)
	if err != nil {
		return err
	}

	return nil
}

var walletBalanceCommand = cli.Command{
	Name:     "walletbalance",
	Category: "Wallet",
	Usage:    "Compute and display the wallet's current balance.",
	Action:   actionDecorator(walletBalance),
}

func walletBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.WalletBalanceRequest{}
	resp, err := client.WalletBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var channelBalanceCommand = cli.Command{
	Name:     "channelbalance",
	Category: "Channels",
	Usage: "Returns the sum of the total available channel balance across " +
		"all open channels.",
	Action: actionDecorator(channelBalance),
}

func channelBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := client.ChannelBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var getInfoCommand = cli.Command{
	Name:   "getinfo",
	Usage:  "Returns basic information related to the active daemon.",
	Action: actionDecorator(getInfo),
}

type chain struct {
	Chain   string `json:"chain"`
	Network string `json:"network"`
}

func getInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetInfoRequest{}
	resp, err := client.GetInfo(ctxb, req)
	if err != nil {
		return err
	}

	chains := make([]chain, len(resp.Chains))
	for i, c := range resp.Chains {
		chains[i] = chain{
			Chain:   c.Chain,
			Network: c.Network,
		}
	}

	// We print a struct that mimics the proto definition of GetInfoResponse
	// but has a better ordering for the same list of fields.
	printJSON(struct {
		Version             string   `json:"version"`
		IdentityPubkey      string   `json:"identity_pubkey"`
		Alias               string   `json:"alias"`
		Color               string   `json:"color"`
		NumPendingChannels  uint32   `json:"num_pending_channels"`
		NumActiveChannels   uint32   `json:"num_active_channels"`
		NumInactiveChannels uint32   `json:"num_inactive_channels"`
		NumPeers            uint32   `json:"num_peers"`
		BlockHeight         uint32   `json:"block_height"`
		BlockHash           string   `json:"block_hash"`
		BestHeaderTimestamp int64    `json:"best_header_timestamp"`
		SyncedToChain       bool     `json:"synced_to_chain"`
		Testnet             bool     `json:"testnet"`
		Chains              []chain  `json:"chains"`
		Uris                []string `json:"uris"`
	}{
		Version:             resp.Version,
		IdentityPubkey:      resp.IdentityPubkey,
		Alias:               resp.Alias,
		Color:               resp.Color,
		NumPendingChannels:  resp.NumPendingChannels,
		NumActiveChannels:   resp.NumActiveChannels,
		NumInactiveChannels: resp.NumInactiveChannels,
		NumPeers:            resp.NumPeers,
		BlockHeight:         resp.BlockHeight,
		BlockHash:           resp.BlockHash,
		BestHeaderTimestamp: resp.BestHeaderTimestamp,
		SyncedToChain:       resp.SyncedToChain,
		Testnet:             resp.Testnet,
		Chains:              chains,
		Uris:                resp.Uris,
	})
	return nil
}

var pendingChannelsCommand = cli.Command{
	Name:     "pendingchannels",
	Category: "Channels",
	Usage:    "Display information pertaining to pending channels.",
	Action:   actionDecorator(pendingChannels),
}

func pendingChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.PendingChannelsRequest{}
	resp, err := client.PendingChannels(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listChannelsCommand = cli.Command{
	Name:     "listchannels",
	Category: "Channels",
	Usage:    "List all open channels.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "active_only",
			Usage: "only list channels which are currently active",
		},
		cli.BoolFlag{
			Name:  "inactive_only",
			Usage: "only list channels which are currently inactive",
		},
		cli.BoolFlag{
			Name:  "public_only",
			Usage: "only list channels which are currently public",
		},
		cli.BoolFlag{
			Name:  "private_only",
			Usage: "only list channels which are currently private",
		},
	},
	Action: actionDecorator(listChannels),
}

func listChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListChannelsRequest{
		ActiveOnly:   ctx.Bool("active_only"),
		InactiveOnly: ctx.Bool("inactive_only"),
		PublicOnly:   ctx.Bool("public_only"),
		PrivateOnly:  ctx.Bool("private_only"),
	}

	resp, err := client.ListChannels(ctxb, req)
	if err != nil {
		return err
	}

	// TODO(roasbeef): defer close the client for the all

	printRespJSON(resp)

	return nil
}

var closedChannelsCommand = cli.Command{
	Name:     "closedchannels",
	Category: "Channels",
	Usage:    "List all closed channels.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "cooperative",
			Usage: "list channels that were closed cooperatively",
		},
		cli.BoolFlag{
			Name: "local_force",
			Usage: "list channels that were force-closed " +
				"by the local node",
		},
		cli.BoolFlag{
			Name: "remote_force",
			Usage: "list channels that were force-closed " +
				"by the remote node",
		},
		cli.BoolFlag{
			Name: "breach",
			Usage: "list channels for which the remote node " +
				"attempted to broadcast a prior " +
				"revoked channel state",
		},
		cli.BoolFlag{
			Name:  "funding_canceled",
			Usage: "list channels that were never fully opened",
		},
		cli.BoolFlag{
			Name: "abandoned",
			Usage: "list channels that were abandoned by " +
				"the local node",
		},
	},
	Action: actionDecorator(closedChannels),
}

func closedChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ClosedChannelsRequest{
		Cooperative:     ctx.Bool("cooperative"),
		LocalForce:      ctx.Bool("local_force"),
		RemoteForce:     ctx.Bool("remote_force"),
		Breach:          ctx.Bool("breach"),
		FundingCanceled: ctx.Bool("funding_cancelled"),
		Abandoned:       ctx.Bool("abandoned"),
	}

	resp, err := client.ClosedChannels(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var cltvLimitFlag = cli.UintFlag{
	Name: "cltv_limit",
	Usage: "the maximum time lock that may be used for " +
		"this payment",
}

// paymentFlags returns common flags for sendpayment and payinvoice.
func paymentFlags() []cli.Flag {
	return []cli.Flag{
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "a zpay32 encoded payment request to fulfill",
		},
		cli.Int64Flag{
			Name: "fee_limit",
			Usage: "maximum fee allowed in satoshis when " +
				"sending the payment",
		},
		cli.Int64Flag{
			Name: "fee_limit_percent",
			Usage: "percentage of the payment's amount used as " +
				"the maximum fee allowed when sending the " +
				"payment",
		},
		cltvLimitFlag,
		cli.Uint64Flag{
			Name: "outgoing_chan_id",
			Usage: "short channel id of the outgoing channel to " +
				"use for the first hop of the payment",
			Value: 0,
		},
		cli.BoolFlag{
			Name:  "force, f",
			Usage: "will skip payment request confirmation",
		},
	}
}

var sendPaymentCommand = cli.Command{
	Name:     "sendpayment",
	Category: "Payments",
	Usage:    "Send a payment over lightning.",
	Description: `
	Send a payment over Lightning. One can either specify the full
	parameters of the payment, or just use a payment request which encodes
	all the payment details.

	If payment isn't manually specified, then only a payment request needs
	to be passed using the --pay_req argument.

	If the payment *is* manually specified, then all four alternative
	arguments need to be specified in order to complete the payment:
	    * --dest=N
	    * --amt=A
	    * --final_cltv_delta=T
	    * --payment_hash=H

	The --debug_send flag is provided for usage *purely* in test
	environments. If specified, then the payment hash isn't required, as
	it'll use the hash of all zeroes. This mode allows one to quickly test
	payment connectivity without having to create an invoice at the
	destination.
	`,
	ArgsUsage: "dest amt payment_hash final_cltv_delta | --pay_req=[payment request]",
	Flags: append(paymentFlags(),
		cli.StringFlag{
			Name: "dest, d",
			Usage: "the compressed identity pubkey of the " +
				"payment recipient",
		},
		cli.Int64Flag{
			Name:  "amt, a",
			Usage: "number of satoshis to send",
		},
		cli.StringFlag{
			Name:  "payment_hash, r",
			Usage: "the hash to use within the payment's HTLC",
		},
		cli.BoolFlag{
			Name:  "debug_send",
			Usage: "use the debug rHash when sending the HTLC",
		},
		cli.Int64Flag{
			Name:  "final_cltv_delta",
			Usage: "the number of blocks the last hop has to reveal the preimage",
		},
	),
	Action: sendPayment,
}

// retrieveFeeLimit retrieves the fee limit based on the different fee limit
// flags passed.
func retrieveFeeLimit(ctx *cli.Context) (*lnrpc.FeeLimit, error) {
	switch {
	case ctx.IsSet("fee_limit") && ctx.IsSet("fee_limit_percent"):
		return nil, fmt.Errorf("either fee_limit or fee_limit_percent " +
			"can be set, but not both")
	case ctx.IsSet("fee_limit"):
		return &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Fixed{
				Fixed: ctx.Int64("fee_limit"),
			},
		}, nil
	case ctx.IsSet("fee_limit_percent"):
		return &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Percent{
				Percent: ctx.Int64("fee_limit_percent"),
			},
		}, nil
	}

	// Since the fee limit flags aren't required, we don't return an error
	// if they're not set.
	return nil, nil
}

func confirmPayReq(resp *lnrpc.PayReq, amt int64) error {
	fmt.Printf("Description: %v\n", resp.GetDescription())
	fmt.Printf("Amount (in satoshis): %v\n", amt)
	fmt.Printf("Destination: %v\n", resp.GetDestination())

	confirm := promptForConfirmation("Confirm payment (yes/no): ")
	if !confirm {
		return fmt.Errorf("payment not confirmed")
	}

	return nil
}

func sendPayment(ctx *cli.Context) error {
	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendpayment")
		return nil
	}

	// If a payment request was provided, we can exit early since all of the
	// details of the payment are encoded within the request.
	if ctx.IsSet("pay_req") {
		req := &lnrpc.SendRequest{
			PaymentRequest: ctx.String("pay_req"),
			Amt:            ctx.Int64("amt"),
		}

		return sendPaymentRequest(ctx, req)
	}

	var (
		destNode []byte
		amount   int64
		err      error
	)

	args := ctx.Args()

	switch {
	case ctx.IsSet("dest"):
		destNode, err = hex.DecodeString(ctx.String("dest"))
	case args.Present():
		destNode, err = hex.DecodeString(args.First())
		args = args.Tail()
	default:
		return fmt.Errorf("destination txid argument missing")
	}
	if err != nil {
		return err
	}

	if len(destNode) != 33 {
		return fmt.Errorf("dest node pubkey must be exactly 33 bytes, is "+
			"instead: %v", len(destNode))
	}

	if ctx.IsSet("amt") {
		amount = ctx.Int64("amt")
	} else if args.Present() {
		amount, err = strconv.ParseInt(args.First(), 10, 64)
		args = args.Tail()
		if err != nil {
			return fmt.Errorf("unable to decode payment amount: %v", err)
		}
	}

	req := &lnrpc.SendRequest{
		Dest: destNode,
		Amt:  amount,
	}

	if ctx.Bool("debug_send") && (ctx.IsSet("payment_hash") || args.Present()) {
		return fmt.Errorf("do not provide a payment hash with debug send")
	} else if !ctx.Bool("debug_send") {
		var rHash []byte

		switch {
		case ctx.IsSet("payment_hash"):
			rHash, err = hex.DecodeString(ctx.String("payment_hash"))
		case args.Present():
			rHash, err = hex.DecodeString(args.First())
			args = args.Tail()
		default:
			return fmt.Errorf("payment hash argument missing")
		}

		if err != nil {
			return err
		}
		if len(rHash) != 32 {
			return fmt.Errorf("payment hash must be exactly 32 "+
				"bytes, is instead %v", len(rHash))
		}
		req.PaymentHash = rHash

		switch {
		case ctx.IsSet("final_cltv_delta"):
			req.FinalCltvDelta = int32(ctx.Int64("final_cltv_delta"))
		case args.Present():
			delta, err := strconv.ParseInt(args.First(), 10, 64)
			if err != nil {
				return err
			}
			req.FinalCltvDelta = int32(delta)
		}
	}

	return sendPaymentRequest(ctx, req)
}

func sendPaymentRequest(ctx *cli.Context, req *lnrpc.SendRequest) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// First, we'll retrieve the fee limit value passed since it can apply
	// to both ways of sending payments (with the payment request or
	// providing the details manually).
	feeLimit, err := retrieveFeeLimit(ctx)
	if err != nil {
		return err
	}
	req.FeeLimit = feeLimit

	req.OutgoingChanId = ctx.Uint64("outgoing_chan_id")
	req.CltvLimit = uint32(ctx.Int(cltvLimitFlag.Name))

	amt := req.Amt

	if req.PaymentRequest != "" {
		req := &lnrpc.PayReqString{PayReq: req.PaymentRequest}
		resp, err := client.DecodePayReq(context.Background(), req)
		if err != nil {
			return err
		}

		invoiceAmt := resp.GetNumSatoshis()
		if invoiceAmt != 0 {
			amt = invoiceAmt
		}

		if !ctx.Bool("force") {
			err := confirmPayReq(resp, amt)
			if err != nil {
				return err
			}
		}
	}

	paymentStream, err := client.SendPayment(context.Background())
	if err != nil {
		return err
	}

	if err := paymentStream.Send(req); err != nil {
		return err
	}

	resp, err := paymentStream.Recv()
	if err != nil {
		return err
	}

	paymentStream.CloseSend()

	printJSON(struct {
		E string       `json:"payment_error"`
		P string       `json:"payment_preimage"`
		R *lnrpc.Route `json:"payment_route"`
	}{
		E: resp.PaymentError,
		P: hex.EncodeToString(resp.PaymentPreimage),
		R: resp.PaymentRoute,
	})

	// If we get a payment error back, we pass an error
	// up to main which eventually calls fatal() and returns
	// with a non-zero exit code.
	if resp.PaymentError != "" {
		return errors.New(resp.PaymentError)
	}

	return nil
}

var payInvoiceCommand = cli.Command{
	Name:      "payinvoice",
	Category:  "Payments",
	Usage:     "Pay an invoice over lightning.",
	ArgsUsage: "pay_req",
	Flags: append(paymentFlags(),
		cli.Int64Flag{
			Name: "amt",
			Usage: "(optional) number of satoshis to fulfill the " +
				"invoice",
		},
	),
	Action: actionDecorator(payInvoice),
}

func payInvoice(ctx *cli.Context) error {
	args := ctx.Args()

	var payReq string
	switch {
	case ctx.IsSet("pay_req"):
		payReq = ctx.String("pay_req")
	case args.Present():
		payReq = args.First()
	default:
		return fmt.Errorf("pay_req argument missing")
	}

	req := &lnrpc.SendRequest{
		PaymentRequest: payReq,
		Amt:            ctx.Int64("amt"),
	}

	return sendPaymentRequest(ctx, req)
}

var sendToRouteCommand = cli.Command{
	Name:     "sendtoroute",
	Category: "Payments",
	Usage:    "Send a payment over a predefined route.",
	Description: `
	Send a payment over Lightning using a specific route. One must specify
	a list of routes to attempt and the payment hash. This command can even
	be chained with the response to queryroutes. This command can be used
	to implement channel rebalancing by crafting a self-route, or even
	atomic swaps using a self-route that crosses multiple chains.

	There are three ways to specify routes:
	   * using the --routes parameter to manually specify a JSON encoded
	     set of routes in the format of the return value of queryroutes:
	         (lncli sendtoroute --payment_hash=<pay_hash> --routes=<route>)

	   * passing the routes as a positional argument:
	         (lncli sendtoroute --payment_hash=pay_hash <route>)

	   * or reading in the routes from stdin, which can allow chaining the
	     response from queryroutes, or even read in a file with a set of
	     pre-computed routes:
	         (lncli queryroutes --args.. | lncli sendtoroute --payment_hash= -

	     notice the '-' at the end, which signals that lncli should read
	     the route in from stdin
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "payment_hash, pay_hash",
			Usage: "the hash to use within the payment's HTLC",
		},
		cli.StringFlag{
			Name: "routes, r",
			Usage: "a json array string in the format of the response " +
				"of queryroutes that denotes which routes to use",
		},
	},
	Action: sendToRoute,
}

func sendToRoute(ctx *cli.Context) error {
	// Show command help if no arguments provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendtoroute")
		return nil
	}

	args := ctx.Args()

	var (
		rHash []byte
		err   error
	)
	switch {
	case ctx.IsSet("payment_hash"):
		rHash, err = hex.DecodeString(ctx.String("payment_hash"))
	case args.Present():
		rHash, err = hex.DecodeString(args.First())

		args = args.Tail()
	default:
		return fmt.Errorf("payment hash argument missing")
	}

	if err != nil {
		return err
	}

	if len(rHash) != 32 {
		return fmt.Errorf("payment hash must be exactly 32 "+
			"bytes, is instead %d", len(rHash))
	}

	var jsonRoutes string
	switch {
	// The user is specifying the routes explicitly via the key word
	// argument.
	case ctx.IsSet("routes"):
		jsonRoutes = ctx.String("routes")

	// The user is specifying the routes as a positional argument.
	case args.Present() && args.First() != "-":
		jsonRoutes = args.First()

	// The user is signalling that we should read stdin in order to parse
	// the set of target routes.
	case args.Present() && args.First() == "-":
		b, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return fmt.Errorf("queryroutes output is empty")
		}

		jsonRoutes = string(b)
	}

	routes := &lnrpc.QueryRoutesResponse{}
	err = jsonpb.UnmarshalString(jsonRoutes, routes)
	if err != nil {
		return fmt.Errorf("unable to unmarshal json string "+
			"from incoming array of routes: %v", err)
	}

	if len(routes.Routes) == 0 {
		return fmt.Errorf("no routes provided")
	}

	if len(routes.Routes) != 1 {
		return fmt.Errorf("expected a single route, but got %v",
			len(routes.Routes))
	}

	req := &lnrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Route:       routes.Routes[0],
	}

	return sendToRouteRequest(ctx, req)
}

func sendToRouteRequest(ctx *cli.Context, req *lnrpc.SendToRouteRequest) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	paymentStream, err := client.SendToRoute(context.Background())
	if err != nil {
		return err
	}

	if err := paymentStream.Send(req); err != nil {
		return err
	}

	resp, err := paymentStream.Recv()
	if err != nil {
		return err
	}

	printJSON(struct {
		E string       `json:"payment_error"`
		P string       `json:"payment_preimage"`
		R *lnrpc.Route `json:"payment_route"`
	}{
		E: resp.PaymentError,
		P: hex.EncodeToString(resp.PaymentPreimage),
		R: resp.PaymentRoute,
	})

	return nil
}

var addInvoiceCommand = cli.Command{
	Name:     "addinvoice",
	Category: "Payments",
	Usage:    "Add a new invoice.",
	Description: `
	Add a new invoice, expressing intent for a future payment.

	Invoices without an amount can be created by not supplying any
	parameters or providing an amount of 0. These invoices allow the payee
	to specify the amount of satoshis they wish to send.`,
	ArgsUsage: "value preimage",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "memo",
			Usage: "a description of the payment to attach along " +
				"with the invoice (default=\"\")",
		},
		cli.StringFlag{
			Name:  "receipt",
			Usage: "an optional cryptographic receipt of payment",
		},
		cli.StringFlag{
			Name: "preimage",
			Usage: "the hex-encoded preimage (32 byte) which will " +
				"allow settling an incoming HTLC payable to this " +
				"preimage. If not set, a random preimage will be " +
				"created.",
		},
		cli.Int64Flag{
			Name:  "amt",
			Usage: "the amt of satoshis in this invoice",
		},
		cli.StringFlag{
			Name: "description_hash",
			Usage: "SHA-256 hash of the description of the payment. " +
				"Used if the purpose of payment cannot naturally " +
				"fit within the memo. If provided this will be " +
				"used instead of the description(memo) field in " +
				"the encoded invoice.",
		},
		cli.StringFlag{
			Name: "fallback_addr",
			Usage: "fallback on-chain address that can be used in " +
				"case the lightning payment fails",
		},
		cli.Int64Flag{
			Name: "expiry",
			Usage: "the invoice's expiry time in seconds. If not " +
				"specified an expiry of 3600 seconds (1 hour) " +
				"is implied.",
		},
		cli.BoolTFlag{
			Name: "private",
			Usage: "encode routing hints in the invoice with " +
				"private channels in order to assist the " +
				"payer in reaching you",
		},
	},
	Action: actionDecorator(addInvoice),
}

func addInvoice(ctx *cli.Context) error {
	var (
		preimage []byte
		descHash []byte
		receipt  []byte
		amt      int64
		err      error
	)

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		args = args.Tail()
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %v", err)
		}
	}

	switch {
	case ctx.IsSet("preimage"):
		preimage, err = hex.DecodeString(ctx.String("preimage"))
	case args.Present():
		preimage, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %v", err)
	}

	descHash, err = hex.DecodeString(ctx.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %v", err)
	}

	receipt, err = hex.DecodeString(ctx.String("receipt"))
	if err != nil {
		return fmt.Errorf("unable to parse receipt: %v", err)
	}

	invoice := &lnrpc.Invoice{
		Memo:            ctx.String("memo"),
		Receipt:         receipt,
		RPreimage:       preimage,
		Value:           amt,
		DescriptionHash: descHash,
		FallbackAddr:    ctx.String("fallback_addr"),
		Expiry:          ctx.Int64("expiry"),
		Private:         ctx.Bool("private"),
	}

	resp, err := client.AddInvoice(context.Background(), invoice)
	if err != nil {
		return err
	}

	printJSON(struct {
		RHash    string `json:"r_hash"`
		PayReq   string `json:"pay_req"`
		AddIndex uint64 `json:"add_index"`
	}{
		RHash:    hex.EncodeToString(resp.RHash),
		PayReq:   resp.PaymentRequest,
		AddIndex: resp.AddIndex,
	})

	return nil
}

var lookupInvoiceCommand = cli.Command{
	Name:      "lookupinvoice",
	Category:  "Payments",
	Usage:     "Lookup an existing invoice by its payment hash.",
	ArgsUsage: "rhash",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "rhash",
			Usage: "the 32 byte payment hash of the invoice to query for, the hash " +
				"should be a hex-encoded string",
		},
	},
	Action: actionDecorator(lookupInvoice),
}

func lookupInvoice(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		rHash []byte
		err   error
	)

	switch {
	case ctx.IsSet("rhash"):
		rHash, err = hex.DecodeString(ctx.String("rhash"))
	case ctx.Args().Present():
		rHash, err = hex.DecodeString(ctx.Args().First())
	default:
		return fmt.Errorf("rhash argument missing")
	}

	if err != nil {
		return fmt.Errorf("unable to decode rhash argument: %v", err)
	}

	req := &lnrpc.PaymentHash{
		RHash: rHash,
	}

	invoice, err := client.LookupInvoice(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJSON(invoice)

	return nil
}

var listInvoicesCommand = cli.Command{
	Name:     "listinvoices",
	Category: "Payments",
	Usage: "List all invoices currently stored within the database. Any " +
		"active debug invoices are ignored.",
	Description: `
	This command enables the retrieval of all invoices currently stored
	within the database. It has full support for paginationed responses,
	allowing users to query for specific invoices through their add_index.
	This can be done by using either the first_index_offset or
	last_index_offset fields included in the response as the index_offset of
	the next request. The reversed flag is set by default in order to
	paginate backwards. If you wish to paginate forwards, you must
	explicitly set the flag to false. If none of the parameters are
	specified, then the last 100 invoices will be returned.

	For example: if you have 200 invoices, "lncli listinvoices" will return
	the last 100 created. If you wish to retrieve the previous 100, the
	first_offset_index of the response can be used as the index_offset of
	the next listinvoices request.`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "pending_only",
			Usage: "toggles if all invoices should be returned, " +
				"or only those that are currently unsettled",
		},
		cli.Uint64Flag{
			Name: "index_offset",
			Usage: "the index of an invoice that will be used as " +
				"either the start or end of a query to " +
				"determine which invoices should be returned " +
				"in the response",
		},
		cli.Uint64Flag{
			Name:  "max_invoices",
			Usage: "the max number of invoices to return",
		},
		cli.BoolTFlag{
			Name: "reversed",
			Usage: "if set, the invoices returned precede the " +
				"given index_offset, allowing backwards " +
				"pagination",
		},
	},
	Action: actionDecorator(listInvoices),
}

func listInvoices(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListInvoiceRequest{
		PendingOnly:    ctx.Bool("pending_only"),
		IndexOffset:    ctx.Uint64("index_offset"),
		NumMaxInvoices: ctx.Uint64("max_invoices"),
		Reversed:       ctx.Bool("reversed"),
	}

	invoices, err := client.ListInvoices(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJSON(invoices)

	return nil
}

var describeGraphCommand = cli.Command{
	Name:     "describegraph",
	Category: "Peers",
	Description: "Prints a human readable version of the known channel " +
		"graph from the PoV of the node",
	Usage: "Describe the network graph.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "include_unannounced",
			Usage: "If set, unannounced channels will be included in the " +
				"graph. Unannounced channels are both private channels, and " +
				"public channels that are not yet announced to the network.",
		},
	},
	Action: actionDecorator(describeGraph),
}

func describeGraph(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: ctx.Bool("include_unannounced"),
	}

	graph, err := client.DescribeGraph(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJSON(graph)
	return nil
}

// normalizeFunc is a factory function which returns a function that normalizes
// the capacity of edges within the graph. The value of the returned
// function can be used to either plot the capacities, or to use a weight in a
// rendering of the graph.
func normalizeFunc(edges []*lnrpc.ChannelEdge, scaleFactor float64) func(int64) float64 {
	var (
		min float64 = math.MaxInt64
		max float64
	)

	for _, edge := range edges {
		// In order to obtain saner values, we reduce the capacity of a
		// channel to its base 2 logarithm.
		z := math.Log2(float64(edge.Capacity))

		if z < min {
			min = z
		}
		if z > max {
			max = z
		}
	}

	return func(x int64) float64 {
		y := math.Log2(float64(x))

		// TODO(roasbeef): results in min being zero
		return (y - min) / (max - min) * scaleFactor
	}
}

var listPaymentsCommand = cli.Command{
	Name:     "listpayments",
	Category: "Payments",
	Usage:    "List all outgoing payments.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "include_incomplete",
			Usage: "if set to true, payments still in flight (or failed) will be returned as well",
		},
	},
	Action: actionDecorator(listPayments),
}

func listPayments(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: ctx.Bool("include_incomplete"),
	}

	payments, err := client.ListPayments(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJSON(payments)
	return nil
}

var getChanInfoCommand = cli.Command{
	Name:     "getchaninfo",
	Category: "Channels",
	Usage:    "Get the state of a channel.",
	Description: "Prints out the latest authenticated state for a " +
		"particular channel",
	ArgsUsage: "chan_id",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "chan_id",
			Usage: "the 8-byte compact channel ID to query for",
		},
	},
	Action: actionDecorator(getChanInfo),
}

func getChanInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		chanID int64
		err    error
	)

	switch {
	case ctx.IsSet("chan_id"):
		chanID = ctx.Int64("chan_id")
	case ctx.Args().Present():
		chanID, err = strconv.ParseInt(ctx.Args().First(), 10, 64)
	default:
		return fmt.Errorf("chan_id argument missing")
	}

	req := &lnrpc.ChanInfoRequest{
		ChanId: uint64(chanID),
	}

	chanInfo, err := client.GetChanInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(chanInfo)
	return nil
}

var getNodeInfoCommand = cli.Command{
	Name:     "getnodeinfo",
	Category: "Peers",
	Usage:    "Get information on a specific node.",
	Description: "Prints out the latest authenticated node state for an " +
		"advertised node",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "pub_key",
			Usage: "the 33-byte hex-encoded compressed public of the target " +
				"node",
		},
		cli.BoolFlag{
			Name: "include_channels",
			Usage: "if true, will return all known channels " +
				"associated with the node",
		},
	},
	Action: actionDecorator(getNodeInfo),
}

func getNodeInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	var pubKey string
	switch {
	case ctx.IsSet("pub_key"):
		pubKey = ctx.String("pub_key")
	case args.Present():
		pubKey = args.First()
	default:
		return fmt.Errorf("pub_key argument missing")
	}

	req := &lnrpc.NodeInfoRequest{
		PubKey:          pubKey,
		IncludeChannels: ctx.Bool("include_channels"),
	}

	nodeInfo, err := client.GetNodeInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(nodeInfo)
	return nil
}

var queryRoutesCommand = cli.Command{
	Name:        "queryroutes",
	Category:    "Payments",
	Usage:       "Query a route to a destination.",
	Description: "Queries the channel router for a potential path to the destination that has sufficient flow for the amount including fees",
	ArgsUsage:   "dest amt",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest",
			Usage: "the 33-byte hex-encoded public key for the payment " +
				"destination",
		},
		cli.Int64Flag{
			Name:  "amt",
			Usage: "the amount to send expressed in satoshis",
		},
		cli.Int64Flag{
			Name: "fee_limit",
			Usage: "maximum fee allowed in satoshis when sending " +
				"the payment",
		},
		cli.Int64Flag{
			Name: "fee_limit_percent",
			Usage: "percentage of the payment's amount used as the " +
				"maximum fee allowed when sending the payment",
		},
		cli.Int64Flag{
			Name: "final_cltv_delta",
			Usage: "(optional) number of blocks the last hop has to reveal " +
				"the preimage",
		},
		cli.BoolFlag{
			Name:  "use_mc",
			Usage: "use mission control probabilities",
		},
	},
	Action: actionDecorator(queryRoutes),
}

func queryRoutes(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		dest string
		amt  int64
		err  error
	)

	args := ctx.Args()

	switch {
	case ctx.IsSet("dest"):
		dest = ctx.String("dest")
	case args.Present():
		dest = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("dest argument missing")
	}

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %v", err)
		}
	default:
		return fmt.Errorf("amt argument missing")
	}

	feeLimit, err := retrieveFeeLimit(ctx)
	if err != nil {
		return err
	}

	req := &lnrpc.QueryRoutesRequest{
		PubKey:            dest,
		Amt:               amt,
		FeeLimit:          feeLimit,
		FinalCltvDelta:    int32(ctx.Int("final_cltv_delta")),
		UseMissionControl: ctx.Bool("use_mc"),
	}

	route, err := client.QueryRoutes(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(route)
	return nil
}

var getNetworkInfoCommand = cli.Command{
	Name:     "getnetworkinfo",
	Category: "Channels",
	Usage: "Get statistical information about the current " +
		"state of the network.",
	Description: "Returns a set of statistics pertaining to the known " +
		"channel graph",
	Action: actionDecorator(getNetworkInfo),
}

func getNetworkInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NetworkInfoRequest{}

	netInfo, err := client.GetNetworkInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(netInfo)
	return nil
}

var debugLevelCommand = cli.Command{
	Name:  "debuglevel",
	Usage: "Set the debug level.",
	Description: `Logging level for all subsystems {trace, debug, info, warn, error, critical, off}
	You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems

	Use show to list available subsystems`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "show",
			Usage: "if true, then the list of available sub-systems will be printed out",
		},
		cli.StringFlag{
			Name:  "level",
			Usage: "the level specification to target either a coarse logging level, or granular set of specific sub-systems with logging levels for each",
		},
	},
	Action: actionDecorator(debugLevel),
}

func debugLevel(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()
	req := &lnrpc.DebugLevelRequest{
		Show:      ctx.Bool("show"),
		LevelSpec: ctx.String("level"),
	}

	resp, err := client.DebugLevel(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var decodePayReqCommand = cli.Command{
	Name:        "decodepayreq",
	Category:    "Payments",
	Usage:       "Decode a payment request.",
	Description: "Decode the passed payment request revealing the destination, payment hash and value of the payment request",
	ArgsUsage:   "pay_req",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "the bech32 encoded payment request",
		},
	},
	Action: actionDecorator(decodePayReq),
}

func decodePayReq(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var payreq string

	switch {
	case ctx.IsSet("pay_req"):
		payreq = ctx.String("pay_req")
	case ctx.Args().Present():
		payreq = ctx.Args().First()
	default:
		return fmt.Errorf("pay_req argument missing")
	}

	resp, err := client.DecodePayReq(ctxb, &lnrpc.PayReqString{
		PayReq: payreq,
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var listChainTxnsCommand = cli.Command{
	Name:        "listchaintxns",
	Category:    "On-chain",
	Usage:       "List transactions from the wallet.",
	Description: "List all transactions an address of the wallet was involved in.",
	Action:      actionDecorator(listChainTxns),
}

func listChainTxns(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	resp, err := client.GetTransactions(ctxb, &lnrpc.GetTransactionsRequest{})

	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var stopCommand = cli.Command{
	Name:  "stop",
	Usage: "Stop and shutdown the daemon.",
	Description: `
	Gracefully stop all daemon subsystems before stopping the daemon itself.
	This is equivalent to stopping it using CTRL-C.`,
	Action: actionDecorator(stopDaemon),
}

func stopDaemon(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	_, err := client.StopDaemon(ctxb, &lnrpc.StopRequest{})
	if err != nil {
		return err
	}

	return nil
}

var signMessageCommand = cli.Command{
	Name:      "signmessage",
	Category:  "Wallet",
	Usage:     "Sign a message with the node's private key.",
	ArgsUsage: "msg",
	Description: `
	Sign msg with the resident node's private key.
	Returns the signature as a zbase32 string.

	Positional arguments and flags can be used interchangeably but not at the same time!`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "msg",
			Usage: "the message to sign",
		},
	},
	Action: actionDecorator(signMessage),
}

func signMessage(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var msg []byte

	switch {
	case ctx.IsSet("msg"):
		msg = []byte(ctx.String("msg"))
	case ctx.Args().Present():
		msg = []byte(ctx.Args().First())
	default:
		return fmt.Errorf("msg argument missing")
	}

	resp, err := client.SignMessage(ctxb, &lnrpc.SignMessageRequest{Msg: msg})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var verifyMessageCommand = cli.Command{
	Name:      "verifymessage",
	Category:  "Wallet",
	Usage:     "Verify a message signed with the signature.",
	ArgsUsage: "msg signature",
	Description: `
	Verify that the message was signed with a properly-formed signature
	The signature must be zbase32 encoded and signed with the private key of
	an active node in the resident node's channel database.

	Positional arguments and flags can be used interchangeably but not at the same time!`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "msg",
			Usage: "the message to verify",
		},
		cli.StringFlag{
			Name:  "sig",
			Usage: "the zbase32 encoded signature of the message",
		},
	},
	Action: actionDecorator(verifyMessage),
}

func verifyMessage(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		msg []byte
		sig string
	)

	args := ctx.Args()

	switch {
	case ctx.IsSet("msg"):
		msg = []byte(ctx.String("msg"))
	case args.Present():
		msg = []byte(ctx.Args().First())
		args = args.Tail()
	default:
		return fmt.Errorf("msg argument missing")
	}

	switch {
	case ctx.IsSet("sig"):
		sig = ctx.String("sig")
	case args.Present():
		sig = args.First()
	default:
		return fmt.Errorf("signature argument missing")
	}

	req := &lnrpc.VerifyMessageRequest{Msg: msg, Signature: sig}
	resp, err := client.VerifyMessage(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var feeReportCommand = cli.Command{
	Name:     "feereport",
	Category: "Channels",
	Usage:    "Display the current fee policies of all active channels.",
	Description: `
	Returns the current fee policies of all active channels.
	Fee policies can be updated using the updatechanpolicy command.`,
	Action: actionDecorator(feeReport),
}

func feeReport(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.FeeReportRequest{}
	resp, err := client.FeeReport(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var updateChannelPolicyCommand = cli.Command{
	Name:     "updatechanpolicy",
	Category: "Channels",
	Usage: "Update the channel policy for all channels, or a single " +
		"channel.",
	ArgsUsage: "base_fee_msat fee_rate time_lock_delta [channel_point]",
	Description: `
	Updates the channel policy for all channels, or just a particular channel
	identified by its channel point. The update will be committed, and
	broadcast to the rest of the network within the next batch.
	Channel points are encoded as: funding_txid:output_index`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "base_fee_msat",
			Usage: "the base fee in milli-satoshis that will " +
				"be charged for each forwarded HTLC, regardless " +
				"of payment size",
		},
		cli.StringFlag{
			Name: "fee_rate",
			Usage: "the fee rate that will be charged " +
				"proportionally based on the value of each " +
				"forwarded HTLC, the lowest possible rate is 0 " +
				"with a granularity of 0.000001 (millionths)",
		},
		cli.Int64Flag{
			Name: "time_lock_delta",
			Usage: "the CLTV delta that will be applied to all " +
				"forwarded HTLCs",
		},
		cli.StringFlag{
			Name: "chan_point",
			Usage: "The channel whose fee policy should be " +
				"updated, if nil the policies for all channels " +
				"will be updated. Takes the form of: txid:output_index",
		},
	},
	Action: actionDecorator(updateChannelPolicy),
}

func parseChanPoint(s string) (*lnrpc.ChannelPoint, error) {
	split := strings.Split(s, ":")
	if len(split) != 2 {
		return nil, fmt.Errorf("expecting chan_point to be in format of: " +
			"txid:index")
	}

	index, err := strconv.ParseInt(split[1], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("unable to decode output index: %v", err)
	}

	txid, err := chainhash.NewHashFromStr(split[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse hex string: %v", err)
	}

	return &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: txid[:],
		},
		OutputIndex: uint32(index),
	}, nil
}

func updateChannelPolicy(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		baseFee       int64
		feeRate       float64
		timeLockDelta int64
		err           error
	)
	args := ctx.Args()

	switch {
	case ctx.IsSet("base_fee_msat"):
		baseFee = ctx.Int64("base_fee_msat")
	case args.Present():
		baseFee, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode base_fee_msat: %v", err)
		}
		args = args.Tail()
	default:
		return fmt.Errorf("base_fee_msat argument missing")
	}

	switch {
	case ctx.IsSet("fee_rate"):
		feeRate = ctx.Float64("fee_rate")
	case args.Present():
		feeRate, err = strconv.ParseFloat(args.First(), 64)
		if err != nil {
			return fmt.Errorf("unable to decode fee_rate: %v", err)
		}

		args = args.Tail()
	default:
		return fmt.Errorf("fee_rate argument missing")
	}

	switch {
	case ctx.IsSet("time_lock_delta"):
		timeLockDelta = ctx.Int64("time_lock_delta")
	case args.Present():
		timeLockDelta, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode time_lock_delta: %v",
				err)
		}

		args = args.Tail()
	default:
		return fmt.Errorf("time_lock_delta argument missing")
	}

	var (
		chanPoint    *lnrpc.ChannelPoint
		chanPointStr string
	)

	switch {
	case ctx.IsSet("chan_point"):
		chanPointStr = ctx.String("chan_point")
	case args.Present():
		chanPointStr = args.First()
	}

	if chanPointStr != "" {
		chanPoint, err = parseChanPoint(chanPointStr)
		if err != nil {
			return fmt.Errorf("unable to parse chan point: %v", err)
		}
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       feeRate,
		TimeLockDelta: uint32(timeLockDelta),
	}

	if chanPoint != nil {
		req.Scope = &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		}
	} else {
		req.Scope = &lnrpc.PolicyUpdateRequest_Global{
			Global: true,
		}
	}

	resp, err := client.UpdateChannelPolicy(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var forwardingHistoryCommand = cli.Command{
	Name:      "fwdinghistory",
	Category:  "Payments",
	Usage:     "Query the history of all forwarded HTLCs.",
	ArgsUsage: "start_time [end_time] [index_offset] [max_events]",
	Description: `
	Query the HTLC switch's internal forwarding log for all completed
	payment circuits (HTLCs) over a particular time range (--start_time and
	--end_time). The start and end times are meant to be expressed in
	seconds since the Unix epoch. If --start_time isn't provided,
	then 24 hours ago is used.  If --end_time isn't provided,
	then the current time is used.

	The max number of events returned is 50k. The default number is 100,
	callers can use the --max_events param to modify this value.

	Finally, callers can skip a series of events using the --index_offset
	parameter. Each response will contain the offset index of the last
	entry. Using this callers can manually paginate within a time slice.
	`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "start_time",
			Usage: "the starting time for the query, expressed in " +
				"seconds since the unix epoch",
		},
		cli.Int64Flag{
			Name: "end_time",
			Usage: "the end time for the query, expressed in " +
				"seconds since the unix epoch",
		},
		cli.Int64Flag{
			Name:  "index_offset",
			Usage: "the number of events to skip",
		},
		cli.Int64Flag{
			Name:  "max_events",
			Usage: "the max number of events to return",
		},
	},
	Action: actionDecorator(forwardingHistory),
}

func forwardingHistory(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		startTime, endTime     uint64
		indexOffset, maxEvents uint32
		err                    error
	)
	args := ctx.Args()

	switch {
	case ctx.IsSet("start_time"):
		startTime = ctx.Uint64("start_time")
	case args.Present():
		startTime, err = strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode start_time %v", err)
		}
		args = args.Tail()
	}

	switch {
	case ctx.IsSet("end_time"):
		endTime = ctx.Uint64("end_time")
	case args.Present():
		endTime, err = strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode end_time: %v", err)
		}
		args = args.Tail()
	}

	switch {
	case ctx.IsSet("index_offset"):
		indexOffset = uint32(ctx.Int64("index_offset"))
	case args.Present():
		i, err := strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode index_offset: %v", err)
		}
		indexOffset = uint32(i)
		args = args.Tail()
	}

	switch {
	case ctx.IsSet("max_events"):
		maxEvents = uint32(ctx.Int64("max_events"))
	case args.Present():
		m, err := strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode max_events: %v", err)
		}
		maxEvents = uint32(m)
		args = args.Tail()
	}

	req := &lnrpc.ForwardingHistoryRequest{
		StartTime:    startTime,
		EndTime:      endTime,
		IndexOffset:  indexOffset,
		NumMaxEvents: maxEvents,
	}
	resp, err := client.ForwardingHistory(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var exportChanBackupCommand = cli.Command{
	Name:     "exportchanbackup",
	Category: "Channels",
	Usage: "Obtain a static channel back up for a selected channels, " +
		"or all known channels",
	ArgsUsage: "[chan_point] [--all] [--output_file]",
	Description: `
	This command allows a user to export a Static Channel Backup (SCB) for
	a selected channel. SCB's are encrypted backups of a channel's initial
	state that are encrypted with a key derived from the seed of a user. In
	the case of partial or complete data loss, the SCB will allow the user
	to reclaim settled funds in the channel at its final state. The
	exported channel backups can be restored at a later time using the
	restorechanbackup command.

	This command will return one of two types of channel backups depending
	on the set of passed arguments:

	   * If a target channel point is specified, then a single channel
	     backup containing only the information for that channel will be
	     returned.

	   * If the --all flag is passed, then a multi-channel backup will be
	     returned. A multi backup is a single encrypted blob (displayed in
	     hex encoding) that contains several channels in a single cipher
	     text.

	Both of the backup types can be restored using the restorechanbackup
	command.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "chan_point",
			Usage: "the target channel to obtain an SCB for",
		},
		cli.BoolFlag{
			Name: "all",
			Usage: "if specified, then a multi backup of all " +
				"active channels will be returned",
		},
		cli.StringFlag{
			Name: "output_file",
			Usage: `
			if specified, then rather than printing a JSON output
			of the static channel backup, a serialized version of
			the backup (either Single or Multi) will be written to
			the target file, this is the same format used by lnd in
			its channels.backup file `,
		},
	},
	Action: actionDecorator(exportChanBackup),
}

func exportChanBackup(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "exportchanbackup")
		return nil
	}

	var (
		err          error
		chanPointStr string
	)
	args := ctx.Args()

	switch {
	case ctx.IsSet("chan_point"):
		chanPointStr = ctx.String("chan_point")

	case args.Present():
		chanPointStr = args.First()

	case !ctx.IsSet("all"):
		return fmt.Errorf("must specify chan_point if --all isn't set")
	}

	if chanPointStr != "" {
		chanPointRPC, err := parseChanPoint(chanPointStr)
		if err != nil {
			return err
		}

		chanBackup, err := client.ExportChannelBackup(
			ctxb, &lnrpc.ExportChannelBackupRequest{
				ChanPoint: chanPointRPC,
			},
		)
		if err != nil {
			return err
		}

		txid, err := chainhash.NewHash(
			chanPointRPC.GetFundingTxidBytes(),
		)
		if err != nil {
			return err
		}

		chanPoint := wire.OutPoint{
			Hash:  *txid,
			Index: chanPointRPC.OutputIndex,
		}

		printJSON(struct {
			ChanPoint  string `json:"chan_point"`
			ChanBackup string `json:"chan_backup"`
		}{
			ChanPoint: chanPoint.String(),
			ChanBackup: hex.EncodeToString(
				chanBackup.ChanBackup,
			),
		},
		)
		return nil
	}

	if !ctx.IsSet("all") {
		return fmt.Errorf("if a channel isn't specified, -all must be")
	}

	chanBackup, err := client.ExportAllChannelBackups(
		ctxb, &lnrpc.ChanBackupExportRequest{},
	)
	if err != nil {
		return err
	}

	if ctx.IsSet("output_file") {
		return ioutil.WriteFile(
			ctx.String("output_file"),
			chanBackup.MultiChanBackup.MultiChanBackup,
			0666,
		)
	}

	// TODO(roasbeef): support for export | restore ?

	var chanPoints []string
	for _, chanPoint := range chanBackup.MultiChanBackup.ChanPoints {
		txid, err := chainhash.NewHash(chanPoint.GetFundingTxidBytes())
		if err != nil {
			return err
		}

		chanPoints = append(chanPoints, wire.OutPoint{
			Hash:  *txid,
			Index: chanPoint.OutputIndex,
		}.String())
	}

	printJSON(struct {
		ChanPoints      []string `json:"chan_points"`
		MultiChanBackup string   `json:"multi_chan_backup"`
	}{
		ChanPoints: chanPoints,
		MultiChanBackup: hex.EncodeToString(
			chanBackup.MultiChanBackup.MultiChanBackup,
		),
	},
	)
	return nil
}

var verifyChanBackupCommand = cli.Command{
	Name:      "verifychanbackup",
	Category:  "Channels",
	Usage:     "Verify an existing channel backup",
	ArgsUsage: "[--single_backup] [--multi_backup] [--multi_file]",
	Description: `
    This command allows a user to verify an existing Single or Multi channel
    backup for integrity. This is useful when a user has a backup, but is
    unsure as to if it's valid or for the target node.

    The command will accept backups in one of three forms:

       * A single channel packed SCB, which can be obtained from
	 exportchanbackup. This should be passed in hex encoded format.

       * A packed multi-channel SCB, which couples several individual
	 static channel backups in single blob.

       * A file path which points to a packed multi-channel backup within a
	 file, using the same format that lnd does in its channels.backup
	 file.
    `,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "single_backup",
			Usage: "a hex encoded single channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name: "multi_backup",
			Usage: "a hex encoded multi-channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name:  "multi_file",
			Usage: "the path to a multi-channel back up file",
		},
	},
	Action: actionDecorator(verifyChanBackup),
}

func verifyChanBackup(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "verifychanbackup")
		return nil
	}

	backups, err := parseChanBackups(ctx)
	if err != nil {
		return err
	}

	verifyReq := lnrpc.ChanBackupSnapshot{}

	if backups.GetChanBackups() != nil {
		verifyReq.SingleChanBackups = backups.GetChanBackups()
	}
	if backups.GetMultiChanBackup() != nil {
		verifyReq.MultiChanBackup = &lnrpc.MultiChanBackup{
			MultiChanBackup: backups.GetMultiChanBackup(),
		}
	}

	resp, err := client.VerifyChanBackup(ctxb, &verifyReq)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var restoreChanBackupCommand = cli.Command{
	Name:     "restorechanbackup",
	Category: "Channels",
	Usage: "Restore an existing single or multi-channel static channel " +
		"backup",
	ArgsUsage: "[--single_backup] [--multi_backup] [--multi_file=",
	Description: `
	Allows a user to restore a Static Channel Backup (SCB) that was
	obtained either via the exportchanbackup command, or from lnd's
	automatically manged channels.backup file. This command should be used
	if a user is attempting to restore a channel due to data loss on a
	running node restored with the same seed as the node that created the
	channel. If successful, this command will allows the user to recover
	the settled funds stored in the recovered channels.

	The command will accept backups in one of three forms:

	   * A single channel packed SCB, which can be obtained from
	     exportchanbackup. This should be passed in hex encoded format.

	   * A packed multi-channel SCB, which couples several individual
	     static channel backups in single blob.

	   * A file path which points to a packed multi-channel backup within a
	     file, using the same format that lnd does in its channels.backup
	     file.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "single_backup",
			Usage: "a hex encoded single channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name: "multi_backup",
			Usage: "a hex encoded multi-channel backup obtained " +
				"from exportchanbackup",
		},
		cli.StringFlag{
			Name:  "multi_file",
			Usage: "the path to a multi-channel back up file",
		},
	},
	Action: actionDecorator(restoreChanBackup),
}

func parseChanBackups(ctx *cli.Context) (*lnrpc.RestoreChanBackupRequest, error) {
	switch {
	case ctx.IsSet("single_backup"):
		packedBackup, err := hex.DecodeString(
			ctx.String("single_backup"),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to decode single packed "+
				"backup: %v", err)
		}

		return &lnrpc.RestoreChanBackupRequest{
			Backup: &lnrpc.RestoreChanBackupRequest_ChanBackups{
				ChanBackups: &lnrpc.ChannelBackups{
					ChanBackups: []*lnrpc.ChannelBackup{
						{
							ChanBackup: packedBackup,
						},
					},
				},
			},
		}, nil

	case ctx.IsSet("multi_backup"):
		packedMulti, err := hex.DecodeString(
			ctx.String("multi_backup"),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to decode multi packed "+
				"backup: %v", err)
		}

		return &lnrpc.RestoreChanBackupRequest{
			Backup: &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
				MultiChanBackup: packedMulti,
			},
		}, nil

	case ctx.IsSet("multi_file"):
		packedMulti, err := ioutil.ReadFile(ctx.String("multi_file"))
		if err != nil {
			return nil, fmt.Errorf("unable to decode multi packed "+
				"backup: %v", err)
		}

		return &lnrpc.RestoreChanBackupRequest{
			Backup: &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
				MultiChanBackup: packedMulti,
			},
		}, nil

	default:
		return nil, nil
	}
}

func restoreChanBackup(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "restorechanbackup")
		return nil
	}

	var req lnrpc.RestoreChanBackupRequest

	backups, err := parseChanBackups(ctx)
	if err != nil {
		return err
	}

	req.Backup = backups.Backup

	_, err = client.RestoreChannelBackups(ctxb, &req)
	if err != nil {
		return fmt.Errorf("unable to restore chan backups: %v", err)
	}

	return nil
}
