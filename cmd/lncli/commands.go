package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/protobuf-hex-display/json"
	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightninglabs/protobuf-hex-display/proto"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TODO(roasbeef): cli logic for supporting both positional and unix style
// arguments.

// TODO(roasbeef): expose all fee conf targets

const defaultRecoveryWindow int32 = 2500

const (
	defaultUtxoMinConf = 1
)

var errBadChanPoint = errors.New("expecting chan_point to be in format of: " +
	"txid:index")

func getContext() context.Context {
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctxc, cancel := context.WithCancel(context.Background())
	go func() {
		<-shutdownInterceptor.ShutdownChannel()
		cancel()
	}()
	return ctxc
}

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
		OrigName:     true,
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
					c.Command.Name == "unlock" ||
					c.Command.Name == "changepassword" ||
					c.Command.Name == "createwatchonly") {

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
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "account",
			Usage: "(optional) the name of the account to " +
				"generate a new address for",
		},
	},
	Description: `
	Generate a wallet new address. Address-types has to be one of:
	    - p2wkh:  Pay to witness key hash
	    - np2wkh: Pay to nested witness key hash
	    - p2tr:   Pay to taproot pubkey`,
	Action: actionDecorator(newAddress),
}

func newAddress(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 || ctx.NumFlags() > 1 {
		return cli.ShowCommandHelp(ctx, "newaddress")
	}

	// Map the string encoded address type, to the concrete typed address
	// type enum. An unrecognized address type will result in an error.
	stringAddrType := ctx.Args().First()
	var addrType lnrpc.AddressType
	switch stringAddrType { // TODO(roasbeef): make them ints on the cli?
	case "p2wkh":
		addrType = lnrpc.AddressType_WITNESS_PUBKEY_HASH
	case "np2wkh":
		addrType = lnrpc.AddressType_NESTED_PUBKEY_HASH
	case "p2tr":
		addrType = lnrpc.AddressType_TAPROOT_PUBKEY
	default:
		return fmt.Errorf("invalid address type %v, support address type "+
			"are: p2wkh, np2wkh, and p2tr", stringAddrType)
	}

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	addr, err := client.NewAddress(ctxc, &lnrpc.NewAddressRequest{
		Type:    addrType,
		Account: ctx.String("account"),
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
	ctxc := getContext()
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().First()
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	resp, err := client.EstimateFee(ctxc, &lnrpc.EstimateFeeRequest{
		AddrToAmount: amountToAddr,
		TargetConf:   int32(ctx.Int64("conf_target")),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var txLabelFlag = cli.StringFlag{
	Name:  "label",
	Usage: "(optional) a label for the transaction",
}

var sendCoinsCommand = cli.Command{
	Name:      "sendcoins",
	Category:  "On-chain",
	Usage:     "Send bitcoin on-chain to an address.",
	ArgsUsage: "addr amt",
	Description: `
	Send amt coins in satoshis to the base58 or bech32 encoded bitcoin address addr.

	Fees used when sending the transaction can be specified via the --conf_target, or
	--sat_per_vbyte optional flags.

	Positional arguments and flags can be used interchangeably but not at the same time!
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "addr",
			Usage: "the base58 or bech32 encoded bitcoin address to send coins " +
				"to on-chain",
		},
		cli.BoolFlag{
			Name: "sweepall",
			Usage: "if set, then the amount field will be ignored, " +
				"and the wallet will attempt to sweep all " +
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
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		cli.Uint64Flag{
			Name: "min_confs",
			Usage: "(optional) the minimum number of confirmations " +
				"each one of your outputs used for the transaction " +
				"must satisfy",
			Value: defaultUtxoMinConf,
		},
		txLabelFlag,
	},
	Action: actionDecorator(sendCoins),
}

func sendCoins(ctx *cli.Context) error {
	var (
		addr string
		amt  int64
		err  error
	)
	ctxc := getContext()
	args := ctx.Args()

	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendcoins")
		return nil
	}

	// Check that only the field sat_per_vbyte or the deprecated field
	// sat_per_byte is used.
	feeRateFlag, err := checkNotBothSet(
		ctx, "sat_per_vbyte", "sat_per_byte",
	)
	if err != nil {
		return err
	}

	// Only fee rate flag or conf_target should be set, not both.
	if _, err := checkNotBothSet(
		ctx, feeRateFlag, "conf_target",
	); err != nil {
		return err
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

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	minConfs := int32(ctx.Uint64("min_confs"))
	req := &lnrpc.SendCoinsRequest{
		Addr:             addr,
		Amount:           amt,
		TargetConf:       int32(ctx.Int64("conf_target")),
		SatPerVbyte:      ctx.Uint64(feeRateFlag),
		SendAll:          ctx.Bool("sweepall"),
		Label:            ctx.String(txLabelFlag.Name),
		MinConfs:         minConfs,
		SpendUnconfirmed: minConfs == 0,
	}
	txid, err := client.SendCoins(ctxc, req)
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
	ctxc := getContext()
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

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListUnspentRequest{
		MinConfs: int32(minConfirms),
		MaxConfs: int32(maxConfirms),
	}
	resp, err := client.ListUnspent(ctxc, req)
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
	ArgsUsage: "send-json-string [--conf_target=N] [--sat_per_vbyte=P]",
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
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		cli.Uint64Flag{
			Name: "min_confs",
			Usage: "(optional) the minimum number of confirmations " +
				"each one of your outputs used for the transaction " +
				"must satisfy",
			Value: defaultUtxoMinConf,
		},
		txLabelFlag,
	},
	Action: actionDecorator(sendMany),
}

func sendMany(ctx *cli.Context) error {
	ctxc := getContext()
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().First()
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	// Check that only the field sat_per_vbyte or the deprecated field
	// sat_per_byte is used.
	feeRateFlag, err := checkNotBothSet(
		ctx, "sat_per_vbyte", "sat_per_byte",
	)
	if err != nil {
		return err
	}

	// Only fee rate flag or conf_target should be set, not both.
	if _, err := checkNotBothSet(
		ctx, feeRateFlag, "conf_target",
	); err != nil {
		return err
	}

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	minConfs := int32(ctx.Uint64("min_confs"))
	txid, err := client.SendMany(ctxc, &lnrpc.SendManyRequest{
		AddrToAmount:     amountToAddr,
		TargetConf:       int32(ctx.Int64("conf_target")),
		SatPerVbyte:      ctx.Uint64(feeRateFlag),
		Label:            ctx.String(txLabelFlag.Name),
		MinConfs:         minConfs,
		SpendUnconfirmed: minConfs == 0,
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
	Description: `
	Connect to a peer using its <pubkey> and host.

	A custom timeout on the connection is supported. For instance, to timeout
	the connection request in 30 seconds, use the following:

	lncli connect <pubkey>@host --timeout 30s
	`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "perm",
			Usage: "If set, the daemon will attempt to persistently " +
				"connect to the target peer.\n" +
				"           If not, the call will be synchronous.",
		},
		cli.DurationFlag{
			Name: "timeout",
			Usage: "The connection timeout value for current request. " +
				"Valid uints are {ms, s, m, h}.\n" +
				"If not set, the global connection " +
				"timeout value (default to 120s) is used.",
		},
	},
	Action: actionDecorator(connectPeer),
}

func connectPeer(ctx *cli.Context) error {
	ctxc := getContext()
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
		Addr:    addr,
		Perm:    ctx.Bool("perm"),
		Timeout: uint64(ctx.Duration("timeout").Seconds()),
	}

	lnid, err := client.ConnectPeer(ctxc, req)
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
	ctxc := getContext()
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

	lnid, err := client.DisconnectPeer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(lnid)
	return nil
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

	In the case of a cooperative closure, one can manually set the fee to
	be used for the closing transaction via either the --conf_target or
	--sat_per_vbyte arguments. This will be the starting value used during
	fee negotiation. This is optional.

	In the case of a cooperative closure, one can manually set the address
	to deliver funds to upon closure. This is optional, and may only be used
	if an upfront shutdown address has not already been set. If neither are
	set the funds will be delivered to a new wallet address.

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
		cli.StringFlag{
			Name: "chan_point",
			Usage: "(optional) the channel point. If set, " +
				"funding_txid and output_index flags and " +
				"positional arguments will be ignored",
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
				"used for fee estimation. If not set, " +
				"then the conf-target value set in the main " +
				"lnd config will be used.",
		},
		cli.Int64Flag{
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		cli.StringFlag{
			Name: "delivery_addr",
			Usage: "(optional) an address to deliver funds " +
				"upon cooperative channel closing, may only " +
				"be used if an upfront shutdown address is not " +
				"already set",
		},
	},
	Action: actionDecorator(closeChannel),
}

func closeChannel(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments and flags were provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "closechannel")
		return nil
	}

	// Check that only the field sat_per_vbyte or the deprecated field
	// sat_per_byte is used.
	feeRateFlag, err := checkNotBothSet(
		ctx, "sat_per_vbyte", "sat_per_byte",
	)
	if err != nil {
		return err
	}

	channelPoint, err := parseChannelPoint(ctx)
	if err != nil {
		return err
	}

	// TODO(roasbeef): implement time deadline within server
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint:    channelPoint,
		Force:           ctx.Bool("force"),
		TargetConf:      int32(ctx.Int64("conf_target")),
		SatPerVbyte:     ctx.Uint64(feeRateFlag),
		DeliveryAddress: ctx.String("delivery_addr"),
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

	err = executeChannelClose(ctxc, client, req, txidChan, ctx.Bool("block"))
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
func executeChannelClose(ctxc context.Context, client lnrpc.LightningClient,
	req *lnrpc.CloseChannelRequest, txidChan chan<- string, block bool) error {

	stream, err := client.CloseChannel(ctxc, req)
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
	--sat_per_vbyte arguments. This will be the starting value used during
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
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the closing transactions",
		},
	},
	Action: actionDecorator(closeAllChannels),
}

func closeAllChannels(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Check that only the field sat_per_vbyte or the deprecated field
	// sat_per_byte is used.
	feeRateFlag, err := checkNotBothSet(
		ctx, "sat_per_vbyte", "sat_per_byte",
	)
	if err != nil {
		return err
	}

	listReq := &lnrpc.ListChannelsRequest{}
	openChannels, err := client.ListChannels(ctxc, listReq)
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
					channel.ChannelPoint, channel.LocalConstraints.CsvDelay)

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
				Force:       !channel.GetActive(),
				TargetConf:  int32(ctx.Int64("conf_target")),
				SatPerVbyte: ctx.Uint64(feeRateFlag),
			}

			txidChan := make(chan string, 1)
			err = executeChannelClose(ctxc, client, req, txidChan, false)
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

	Only available when lnd is built in debug mode. The flag
	--i_know_what_i_am_doing can be set to override the debug/dev mode
	requirement.

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
		cli.StringFlag{
			Name: "chan_point",
			Usage: "(optional) the channel point. If set, " +
				"funding_txid and output_index flags and " +
				"positional arguments will be ignored",
		},
		cli.BoolFlag{
			Name: "i_know_what_i_am_doing",
			Usage: "override the requirement for lnd needing to " +
				"be in dev/debug mode to use this command; " +
				"when setting this the user attests that " +
				"they know the danger of using this command " +
				"on channels and that doing so can lead to " +
				"loss of funds if the channel funding TX " +
				"ever confirms (or was confirmed)",
		},
	},
	Action: actionDecorator(abandonChannel),
}

func abandonChannel(ctx *cli.Context) error {
	ctxc := getContext()
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
		ChannelPoint:      channelPoint,
		IKnowWhatIAmDoing: ctx.Bool("i_know_what_i_am_doing"),
	}

	resp, err := client.AbandonChannel(ctxc, req)
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
	var err error

	args := ctx.Args()

	switch {
	case ctx.IsSet("chan_point"):
		channelPoint, err = parseChanPoint(ctx.String("chan_point"))
		if err != nil {
			return nil, fmt.Errorf("unable to parse chan_point: "+
				"%v", err)
		}
		return channelPoint, nil

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
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "list_errors",
			Usage: "list a full set of most recent errors for the peer",
		},
	},
	Action: actionDecorator(listPeers),
}

func listPeers(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// By default, we display a single error on the cli. If the user
	// specifically requests a full error set, then we will provide it.
	req := &lnrpc.ListPeersRequest{
		LatestError: !ctx.IsSet("list_errors"),
	}
	resp, err := client.ListPeers(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var walletBalanceCommand = cli.Command{
	Name:     "walletbalance",
	Category: "Wallet",
	Usage:    "Compute and display the wallet's current balance.",
	Action:   actionDecorator(walletBalance),
}

func walletBalance(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.WalletBalanceRequest{}
	resp, err := client.WalletBalance(ctxc, req)
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := client.ChannelBalance(ctxc, req)
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

func getInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetInfoRequest{}
	resp, err := client.GetInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var getRecoveryInfoCommand = cli.Command{
	Name:   "getrecoveryinfo",
	Usage:  "Display information about an ongoing recovery attempt.",
	Action: actionDecorator(getRecoveryInfo),
}

func getRecoveryInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetRecoveryInfoRequest{}
	resp, err := client.GetRecoveryInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var pendingChannelsCommand = cli.Command{
	Name:     "pendingchannels",
	Category: "Channels",
	Usage:    "Display information pertaining to pending channels.",
	Action:   actionDecorator(pendingChannels),
}

func pendingChannels(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.PendingChannelsRequest{}
	resp, err := client.PendingChannels(ctxc, req)
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
		cli.StringFlag{
			Name: "peer",
			Usage: "(optional) only display channels with a " +
				"particular peer, accepts 66-byte, " +
				"hex-encoded pubkeys",
		},
		cli.BoolFlag{
			Name: "skip_peer_alias_lookup",
			Usage: "skip the peer alias lookup per channel in " +
				"order to improve performance",
		},
	},
	Action: actionDecorator(listChannels),
}

var listAliasesCommand = cli.Command{
	Name:     "listaliases",
	Category: "Channels",
	Usage:    "List all aliases.",
	Flags:    []cli.Flag{},
	Action:   actionDecorator(listaliases),
}

func listaliases(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListAliasesRequest{}

	resp, err := client.ListAliases(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func listChannels(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	peer := ctx.String("peer")

	// If the user requested channels with a particular key, parse the
	// provided pubkey.
	var peerKey []byte
	if len(peer) > 0 {
		pk, err := route.NewVertexFromStr(peer)
		if err != nil {
			return fmt.Errorf("invalid --peer pubkey: %v", err)
		}

		peerKey = pk[:]
	}

	// By default we will look up the peers' alias information unless the
	// skip_peer_alias_lookup flag indicates otherwise.
	lookupPeerAlias := !ctx.Bool("skip_peer_alias_lookup")

	req := &lnrpc.ListChannelsRequest{
		ActiveOnly:      ctx.Bool("active_only"),
		InactiveOnly:    ctx.Bool("inactive_only"),
		PublicOnly:      ctx.Bool("public_only"),
		PrivateOnly:     ctx.Bool("private_only"),
		Peer:            peerKey,
		PeerAliasLookup: lookupPeerAlias,
	}

	resp, err := client.ListChannels(ctxc, req)
	if err != nil {
		return err
	}

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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ClosedChannelsRequest{
		Cooperative:     ctx.Bool("cooperative"),
		LocalForce:      ctx.Bool("local_force"),
		RemoteForce:     ctx.Bool("remote_force"),
		Breach:          ctx.Bool("breach"),
		FundingCanceled: ctx.Bool("funding_canceled"),
		Abandoned:       ctx.Bool("abandoned"),
	}

	resp, err := client.ClosedChannels(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var describeGraphCommand = cli.Command{
	Name:     "describegraph",
	Category: "Graph",
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: ctx.Bool("include_unannounced"),
	}

	graph, err := client.DescribeGraph(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(graph)
	return nil
}

var getNodeMetricsCommand = cli.Command{
	Name:        "getnodemetrics",
	Category:    "Graph",
	Description: "Prints out node metrics calculated from the current graph",
	Usage:       "Get node metrics.",
	Action:      actionDecorator(getNodeMetrics),
}

func getNodeMetrics(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NodeMetricsRequest{
		Types: []lnrpc.NodeMetricType{lnrpc.NodeMetricType_BETWEENNESS_CENTRALITY},
	}

	nodeMetrics, err := client.GetNodeMetrics(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(nodeMetrics)
	return nil
}

var getChanInfoCommand = cli.Command{
	Name:     "getchaninfo",
	Category: "Graph",
	Usage:    "Get the state of a channel.",
	Description: "Prints out the latest authenticated state for a " +
		"particular channel",
	ArgsUsage: "chan_id",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "chan_id",
			Usage: "the 8-byte compact channel ID to query for",
		},
	},
	Action: actionDecorator(getChanInfo),
}

func getChanInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		chanID uint64
		err    error
	)

	switch {
	case ctx.IsSet("chan_id"):
		chanID = ctx.Uint64("chan_id")
	case ctx.Args().Present():
		chanID, err = strconv.ParseUint(ctx.Args().First(), 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing chan_id: %s", err)
		}
	default:
		return fmt.Errorf("chan_id argument missing")
	}

	req := &lnrpc.ChanInfoRequest{
		ChanId: chanID,
	}

	chanInfo, err := client.GetChanInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(chanInfo)
	return nil
}

var getNodeInfoCommand = cli.Command{
	Name:     "getnodeinfo",
	Category: "Graph",
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
	ctxc := getContext()
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

	nodeInfo, err := client.GetNodeInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(nodeInfo)
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NetworkInfoRequest{}

	netInfo, err := client.GetNetworkInfo(ctxc, req)
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()
	req := &lnrpc.DebugLevelRequest{
		Show:      ctx.Bool("show"),
		LevelSpec: ctx.String("level"),
	}

	resp, err := client.DebugLevel(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var listChainTxnsCommand = cli.Command{
	Name:     "listchaintxns",
	Category: "On-chain",
	Usage:    "List transactions from the wallet.",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "start_height",
			Usage: "the block height from which to list " +
				"transactions, inclusive",
		},
		cli.Int64Flag{
			Name: "end_height",
			Usage: "the block height until which to list " +
				"transactions, inclusive, to get transactions " +
				"until the chain tip, including unconfirmed, " +
				"set this value to -1",
		},
	},
	Description: `
	List all transactions an address of the wallet was involved in.

	This call will return a list of wallet related transactions that paid
	to an address our wallet controls, or spent utxos that we held. The
	start_height and end_height flags can be used to specify an inclusive
	block range over which to query for transactions. If the end_height is
	less than the start_height, transactions will be queried in reverse.
	To get all transactions until the chain tip, including unconfirmed
	transactions (identifiable with BlockHeight=0), set end_height to -1.
	By default, this call will get all transactions our wallet was involved
	in, including unconfirmed transactions.
`,
	Action: actionDecorator(listChainTxns),
}

func listChainTxns(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetTransactionsRequest{}

	if ctx.IsSet("start_height") {
		req.StartHeight = int32(ctx.Int64("start_height"))
	}
	if ctx.IsSet("end_height") {
		req.EndHeight = int32(ctx.Int64("end_height"))
	}

	resp, err := client.GetTransactions(ctxc, req)
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	_, err := client.StopDaemon(ctxc, &lnrpc.StopRequest{})
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
	ctxc := getContext()
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

	resp, err := client.SignMessage(ctxc, &lnrpc.SignMessageRequest{Msg: msg})
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
	ctxc := getContext()
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
	resp, err := client.VerifyMessage(ctxc, req)
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
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.FeeReportRequest{}
	resp, err := client.FeeReport(ctxc, req)
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
	ArgsUsage: "base_fee_msat fee_rate time_lock_delta " +
		"[--max_htlc_msat=N] [channel_point]",
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
				"with a granularity of 0.000001 (millionths). Can not " +
				"be set at the same time as fee_rate_ppm.",
		},
		cli.Uint64Flag{
			Name: "fee_rate_ppm",
			Usage: "the fee rate ppm (parts per million) that " +
				"will be charged proportionally based on the value of each " +
				"forwarded HTLC, the lowest possible rate is 0 " +
				"with a granularity of 0.000001 (millionths). Can not " +
				"be set at the same time as fee_rate.",
		},
		cli.Uint64Flag{
			Name: "time_lock_delta",
			Usage: "the CLTV delta that will be applied to all " +
				"forwarded HTLCs",
		},
		cli.Uint64Flag{
			Name: "min_htlc_msat",
			Usage: "if set, the min HTLC size that will be applied " +
				"to all forwarded HTLCs. If unset, the min HTLC " +
				"is left unchanged.",
		},
		cli.Uint64Flag{
			Name: "max_htlc_msat",
			Usage: "if set, the max HTLC size that will be applied " +
				"to all forwarded HTLCs. If unset, the max HTLC " +
				"is left unchanged.",
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
	if len(split) != 2 || len(split[0]) == 0 || len(split[1]) == 0 {
		return nil, errBadChanPoint
	}

	index, err := strconv.ParseInt(split[1], 10, 64)
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

// parseTimeLockDelta is expected to get a uint16 type of timeLockDelta,
// which maximum value is MaxTimeLockDelta.
func parseTimeLockDelta(timeLockDeltaStr string) (uint16, error) {
	timeLockDeltaUnCheck, err := strconv.ParseUint(
		timeLockDeltaStr, 10, 64,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to parse time_lock_delta: %s "+
			"to uint64, err: %v", timeLockDeltaStr, err)
	}

	if timeLockDeltaUnCheck > routing.MaxCLTVDelta {
		return 0, fmt.Errorf("time_lock_delta is too big, "+
			"max value is %d", routing.MaxCLTVDelta)
	}

	return uint16(timeLockDeltaUnCheck), nil
}

func updateChannelPolicy(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		baseFee       int64
		feeRate       float64
		feeRatePpm    uint64
		timeLockDelta uint16
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
	case ctx.IsSet("fee_rate") && ctx.IsSet("fee_rate_ppm"):
		return fmt.Errorf("fee_rate or fee_rate_ppm can not both be set")
	case ctx.IsSet("fee_rate"):
		feeRate = ctx.Float64("fee_rate")
	case ctx.IsSet("fee_rate_ppm"):
		feeRatePpm = ctx.Uint64("fee_rate_ppm")
	case args.Present():
		feeRate, err = strconv.ParseFloat(args.First(), 64)
		if err != nil {
			return fmt.Errorf("unable to decode fee_rate: %v", err)
		}

		args = args.Tail()
	default:
		return fmt.Errorf("fee_rate or fee_rate_ppm argument missing")
	}

	switch {
	case ctx.IsSet("time_lock_delta"):
		timeLockDeltaStr := ctx.String("time_lock_delta")
		timeLockDelta, err = parseTimeLockDelta(timeLockDeltaStr)
		if err != nil {
			return err
		}
	case args.Present():
		timeLockDelta, err = parseTimeLockDelta(args.First())
		if err != nil {
			return err
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
			return fmt.Errorf("unable to parse chan_point: %v", err)
		}
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		TimeLockDelta: uint32(timeLockDelta),
		MaxHtlcMsat:   ctx.Uint64("max_htlc_msat"),
	}

	if ctx.IsSet("min_htlc_msat") {
		req.MinHtlcMsat = ctx.Uint64("min_htlc_msat")
		req.MinHtlcMsatSpecified = true
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

	if feeRate != 0 {
		req.FeeRate = feeRate
	} else if feeRatePpm != 0 {
		req.FeeRatePpm = uint32(feeRatePpm)
	}

	resp, err := client.UpdateChannelPolicy(ctxc, req)
	if err != nil {
		return err
	}

	// Parse the response into the final json object that will be printed
	// to stdout. At the moment, this filters out the raw txid bytes from
	// each failed update's outpoint and only prints the txid string.
	var listFailedUpdateResp = struct {
		FailedUpdates []*FailedUpdate `json:"failed_updates"`
	}{
		FailedUpdates: make([]*FailedUpdate, 0, len(resp.FailedUpdates)),
	}
	for _, protoUpdate := range resp.FailedUpdates {
		failedUpdate := NewFailedUpdateFromProto(protoUpdate)
		listFailedUpdateResp.FailedUpdates = append(
			listFailedUpdateResp.FailedUpdates, failedUpdate)
	}

	printJSON(listFailedUpdateResp)

	return nil
}

var fishCompletionCommand = cli.Command{
	Name:   "fish-completion",
	Hidden: true,
	Action: func(c *cli.Context) error {
		completion, err := c.App.ToFishCompletion()
		if err != nil {
			return err
		}

		// We don't want to suggest files, so we add this
		// first line to the completions.
		_, err = fmt.Printf("complete -c %q -f \n%s", c.App.Name, completion)
		return err
	},
}

var exportChanBackupCommand = cli.Command{
	Name:     "exportchanbackup",
	Category: "Channels",
	Usage: "Obtain a static channel back up for a selected channels, " +
		"or all known channels.",
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
			its channel.backup file `,
		},
	},
	Action: actionDecorator(exportChanBackup),
}

func exportChanBackup(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "exportchanbackup")
		return nil
	}

	var (
		err            error
		chanPointStr   string
		outputFileName string
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

	if ctx.IsSet("output_file") {
		outputFileName = ctx.String("output_file")
	}

	if chanPointStr != "" {
		chanPointRPC, err := parseChanPoint(chanPointStr)
		if err != nil {
			return fmt.Errorf("unable to parse chan_point: %v", err)
		}

		chanBackup, err := client.ExportChannelBackup(
			ctxc, &lnrpc.ExportChannelBackupRequest{
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

		if outputFileName != "" {
			return os.WriteFile(
				outputFileName,
				chanBackup.ChanBackup,
				0666,
			)
		}

		printJSON(struct {
			ChanPoint  string `json:"chan_point"`
			ChanBackup []byte `json:"chan_backup"`
		}{
			ChanPoint:  chanPoint.String(),
			ChanBackup: chanBackup.ChanBackup,
		})
		return nil
	}

	if !ctx.IsSet("all") {
		return fmt.Errorf("if a channel isn't specified, -all must be")
	}

	chanBackup, err := client.ExportAllChannelBackups(
		ctxc, &lnrpc.ChanBackupExportRequest{},
	)
	if err != nil {
		return err
	}

	if outputFileName != "" {
		return os.WriteFile(
			outputFileName,
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

	printRespJSON(chanBackup)

	return nil
}

var verifyChanBackupCommand = cli.Command{
	Name:      "verifychanbackup",
	Category:  "Channels",
	Usage:     "Verify an existing channel backup.",
	ArgsUsage: "[--single_backup] [--multi_backup] [--multi_file]",
	Description: `
    This command allows a user to verify an existing Single or Multi channel
    backup for integrity. This is useful when a user has a backup, but is
    unsure as to if it's valid or for the target node.

    The command will accept backups in one of four forms:

       * A single channel packed SCB, which can be obtained from
	 exportchanbackup. This should be passed in hex encoded format.

       * A packed multi-channel SCB, which couples several individual
	 static channel backups in single blob.
	
       * A file path which points to a packed single-channel backup within a
         file, using the same format that lnd does in its channel.backup file.

       * A file path which points to a packed multi-channel backup within a
	 file, using the same format that lnd does in its channel.backup
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
			Name:      "single_file",
			Usage:     "the path to a single-channel backup file",
			TakesFile: true,
		},

		cli.StringFlag{
			Name:      "multi_file",
			Usage:     "the path to a multi-channel back up file",
			TakesFile: true,
		},
	},
	Action: actionDecorator(verifyChanBackup),
}

func verifyChanBackup(ctx *cli.Context) error {
	ctxc := getContext()
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

	resp, err := client.VerifyChanBackup(ctxc, &verifyReq)
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
		"backup.",
	ArgsUsage: "[--single_backup] [--multi_backup] [--multi_file=",
	Description: `
	Allows a user to restore a Static Channel Backup (SCB) that was
	obtained either via the exportchanbackup command, or from lnd's
	automatically managed channel.backup file. This command should be used
	if a user is attempting to restore a channel due to data loss on a
	running node restored with the same seed as the node that created the
	channel. If successful, this command will allows the user to recover
	the settled funds stored in the recovered channels.

	The command will accept backups in one of four forms:

	   * A single channel packed SCB, which can be obtained from
	     exportchanbackup. This should be passed in hex encoded format.

	   * A packed multi-channel SCB, which couples several individual
	     static channel backups in single blob.

	   * A file path which points to a packed single-channel backup within
	     a file, using the same format that lnd does in its channel.backup
	     file.

	   * A file path which points to a packed multi-channel backup within a
	     file, using the same format that lnd does in its channel.backup
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
			Name:      "single_file",
			Usage:     "the path to a single-channel backup file",
			TakesFile: true,
		},

		cli.StringFlag{
			Name:      "multi_file",
			Usage:     "the path to a multi-channel back up file",
			TakesFile: true,
		},
	},
	Action: actionDecorator(restoreChanBackup),
}

// errMissingChanBackup is an error returned when we attempt to parse a channel
// backup from a CLI command and it is missing.
var errMissingChanBackup = errors.New("missing channel backup")

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

	case ctx.IsSet("single_file"):
		packedSingle, err := os.ReadFile(ctx.String("single_file"))
		if err != nil {
			return nil, fmt.Errorf("unable to decode single "+
				"packed backup: %v", err)
		}

		return &lnrpc.RestoreChanBackupRequest{
			Backup: &lnrpc.RestoreChanBackupRequest_ChanBackups{
				ChanBackups: &lnrpc.ChannelBackups{
					ChanBackups: []*lnrpc.ChannelBackup{{
						ChanBackup: packedSingle,
					}},
				},
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
		return nil, errMissingChanBackup
	}
}

func restoreChanBackup(ctx *cli.Context) error {
	ctxc := getContext()
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

	_, err = client.RestoreChannelBackups(ctxc, &req)
	if err != nil {
		return fmt.Errorf("unable to restore chan backups: %v", err)
	}

	return nil
}
