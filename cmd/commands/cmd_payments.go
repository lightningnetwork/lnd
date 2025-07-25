package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	// paymentTimeout is the default timeout for the payment loop in lnd.
	// No new attempts will be started after the timeout.
	paymentTimeout = time.Second * 60
)

var (
	cltvLimitFlag = cli.UintFlag{
		Name: "cltv_limit",
		Usage: "the maximum time lock that may be used for " +
			"this payment",
	}

	lastHopFlag = cli.StringFlag{
		Name: "last_hop",
		Usage: "pubkey of the last hop (penultimate node in the path) " +
			"to route through for this payment",
	}

	dataFlag = cli.StringFlag{
		Name: "data",
		Usage: "attach custom data to the payment. The required " +
			"format is: <record_id>=<hex_value>,<record_id>=" +
			"<hex_value>,.. For example: --data 3438382=0a21ff. " +
			"Custom record ids start from 65536.",
	}

	inflightUpdatesFlag = cli.BoolFlag{
		Name: "inflight_updates",
		Usage: "if set, intermediate payment state updates will be " +
			"displayed. Only valid in combination with --json.",
	}

	maxPartsFlag = cli.UintFlag{
		Name: "max_parts",
		Usage: "the maximum number of partial payments that may be " +
			"used",
		Value: routerrpc.DefaultMaxParts,
	}

	jsonFlag = cli.BoolFlag{
		Name: "json",
		Usage: "if set, payment updates are printed as json " +
			"messages. Set by default on Windows because table " +
			"formatting is unsupported.",
	}

	maxShardSizeSatFlag = cli.UintFlag{
		Name: "max_shard_size_sat",
		Usage: "the largest payment split that should be attempted if " +
			"payment splitting is required to attempt a payment, " +
			"specified in satoshis",
	}

	maxShardSizeMsatFlag = cli.UintFlag{
		Name: "max_shard_size_msat",
		Usage: "the largest payment split that should be attempted if " +
			"payment splitting is required to attempt a payment, " +
			"specified in milli-satoshis",
	}

	ampFlag = cli.BoolFlag{
		Name: "amp",
		Usage: "if set to true, then AMP will be used to complete the " +
			"payment",
	}

	timePrefFlag = cli.Float64Flag{
		Name:  "time_pref",
		Usage: "(optional) expresses time preference (range -1 to 1)",
	}

	introductionNodeFlag = cli.StringFlag{
		Name: "introduction_node",
		Usage: "(blinded paths) the hex encoded, cleartext node ID " +
			"of the node to use for queries to a blinded route",
	}

	blindingPointFlag = cli.StringFlag{
		Name: "blinding_point",
		Usage: "(blinded paths) the hex encoded blinding point to " +
			"use if querying a route to a blinded path, this " +
			"value *must* be set for queries to a blinded path",
	}

	blindedHopsFlag = cli.StringSliceFlag{
		Name: "blinded_hops",
		Usage: "(blinded paths) the blinded hops to include in the " +
			"query, formatted as <blinded_node_id>:" +
			"<hex_encrypted_data>. These hops must be provided " +
			"*in order* starting with the introduction point and " +
			"ending with the receiving node",
	}

	blindedBaseFlag = cli.Uint64Flag{
		Name: "blinded_base_fee",
		Usage: "(blinded paths) the aggregate base fee for the " +
			"blinded portion of the route, expressed in msat",
	}

	blindedPPMFlag = cli.Uint64Flag{
		Name: "blinded_ppm_fee",
		Usage: "(blinded paths) the aggregate proportional fee for " +
			"the blinded portion of the route, expressed in " +
			"parts per million",
	}

	blindedCLTVFlag = cli.Uint64Flag{
		Name: "blinded_cltv",
		Usage: "(blinded paths) the total cltv delay for the " +
			"blinded portion of the route",
	}

	cancelableFlag = cli.BoolFlag{
		Name: "cancelable",
		Usage: "if set to true, the payment loop can be interrupted " +
			"by manually canceling the payment context, even " +
			"before the payment timeout is reached. Note that " +
			"the payment may still succeed after cancellation, " +
			"as in-flight attempts can still settle afterwards. " +
			"Canceling will only prevent further attempts from " +
			"being sent",
	}
)

// PaymentFlags returns common flags for sendpayment and payinvoice.
func PaymentFlags() []cli.Flag {
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
		cli.DurationFlag{
			Name: "timeout",
			Usage: "the maximum amount of time we should spend " +
				"trying to fulfill the payment, failing " +
				"after the timeout has elapsed",
			Value: paymentTimeout,
		},
		cancelableFlag,
		cltvLimitFlag,
		lastHopFlag,
		cli.StringSliceFlag{
			Name: "outgoing_chan_id",
			Usage: "short channel id of the outgoing channel to " +
				"use for the first hop of the payment; can " +
				"be specified multiple times in the same " +
				"command",
			Value: &cli.StringSlice{},
		},
		cli.BoolFlag{
			Name:  "force, f",
			Usage: "will skip payment request confirmation",
		},
		cli.BoolFlag{
			Name:  "allow_self_payment",
			Usage: "allow sending a circular payment to self",
		},
		dataFlag, inflightUpdatesFlag, maxPartsFlag, jsonFlag,
		maxShardSizeSatFlag, maxShardSizeMsatFlag, ampFlag,
		timePrefFlag,
	}
}

var SendPaymentCommand = cli.Command{
	Name:     "sendpayment",
	Category: "Payments",
	Usage:    "Send a payment over lightning.",
	Description: `
	Send a payment over Lightning. One can either specify the full
	parameters of the payment, or just use a payment request which encodes
	all the payment details.

	If payment isn't manually specified, then only a payment request needs
	to be passed using the --pay_req argument.

	If the payment *is* manually specified, then the following arguments
	need to be specified in order to complete the payment:

	For invoice with keysend,
	    --dest=N --amt=A --final_cltv_delta=T --keysend
	For invoice without payment address:
	    --dest=N --amt=A --payment_hash=H --final_cltv_delta=T
	For invoice with payment address:
	    --dest=N --amt=A --payment_hash=H --final_cltv_delta=T --pay_addr=H
	`,
	ArgsUsage: "dest amt payment_hash final_cltv_delta pay_addr | " +
		"--pay_req=R [--pay_addr=H]",
	Flags: append(PaymentFlags(),
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
		cli.Int64Flag{
			Name:  "final_cltv_delta",
			Usage: "the number of blocks the last hop has to reveal the preimage",
		},
		cli.StringFlag{
			Name:  "pay_addr",
			Usage: "the payment address of the generated invoice",
		},
		cli.BoolFlag{
			Name:  "keysend",
			Usage: "will generate a pre-image and encode it in the sphinx packet, a dest must be set [experimental]",
		},
		cli.StringFlag{
			Name: "route_hints",
			Usage: `route hints for sending through private ` +
				`channels. eg: ` +
				`'[{"hop_hints":[{"node_id":"A","chan_id":1,` +
				`"fee_base_msat":2,` +
				`"fee_proportional_millionths":3,` +
				`"cltv_expiry_delta":4}]}]'`,
		},
	),
	Action: SendPayment,
}

// retrieveFeeLimit retrieves the fee limit based on the different fee limit
// flags passed. It always returns a value and doesn't rely on lnd applying a
// default.
func retrieveFeeLimit(ctx *cli.Context, amt int64) (int64, error) {
	switch {
	case ctx.IsSet("fee_limit") && ctx.IsSet("fee_limit_percent"):
		return 0, fmt.Errorf("either fee_limit or fee_limit_percent " +
			"can be set, but not both")

	case ctx.IsSet("fee_limit"):
		return ctx.Int64("fee_limit"), nil

	case ctx.IsSet("fee_limit_percent"):
		// Round up the fee limit to prevent hitting zero on small
		// amounts.
		feeLimitRoundedUp :=
			(amt*ctx.Int64("fee_limit_percent") + 99) / 100

		return feeLimitRoundedUp, nil
	}

	// If no fee limit is set, use a default value based on the amount.
	amtMsat := lnwire.NewMSatFromSatoshis(btcutil.Amount(amt))
	limitMsat := lnwallet.DefaultRoutingFeeLimitForAmount(amtMsat)
	return int64(limitMsat.ToSatoshis()), nil
}

func confirmPayReq(resp *lnrpc.PayReq, amt, feeLimit int64) error {
	fmt.Printf("Payment hash: %v\n", resp.GetPaymentHash())
	fmt.Printf("Description: %v\n", resp.GetDescription())
	fmt.Printf("Amount (in satoshis): %v\n", amt)
	fmt.Printf("Fee limit (in satoshis): %v\n", feeLimit)
	fmt.Printf("Destination: %v\n", resp.GetDestination())

	confirm := promptForConfirmation("Confirm payment (yes/no): ")
	if !confirm {
		return fmt.Errorf("payment not confirmed")
	}

	return nil
}

func parsePayAddr(ctx *cli.Context, args cli.Args) ([]byte, error) {
	var (
		payAddr []byte
		err     error
	)
	switch {
	case ctx.IsSet("pay_addr"):
		payAddr, err = hex.DecodeString(ctx.String("pay_addr"))

	case args.Present():
		payAddr, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return nil, err
	}

	// payAddr may be not required if it's a legacy invoice.
	if len(payAddr) != 0 && len(payAddr) != 32 {
		return nil, fmt.Errorf("payment addr must be exactly 32 "+
			"bytes, is instead %v", len(payAddr))
	}

	return payAddr, nil
}

func SendPayment(ctx *cli.Context) error {
	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "sendpayment")
		return nil
	}

	conn := getClientConn(ctx, false)
	defer conn.Close()

	args := ctx.Args()

	// If a payment request was provided, we can exit early since all of the
	// details of the payment are encoded within the request.
	if ctx.IsSet("pay_req") {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest:    StripPrefix(ctx.String("pay_req")),
			Amt:               ctx.Int64("amt"),
			DestCustomRecords: make(map[uint64][]byte),
			Amp:               ctx.Bool(ampFlag.Name),
			Cancelable:        ctx.Bool(cancelableFlag.Name),
		}

		// We'll attempt to parse a payment address as well, given that
		// if the user is using an AMP invoice, then they may be trying
		// to specify that value manually.
		//
		// Don't parse unnamed arguments to prevent confusion with the
		// main unnamed argument format for non-AMP payments.
		payAddr, err := parsePayAddr(ctx, nil)
		if err != nil {
			return err
		}

		req.PaymentAddr = payAddr

		return SendPaymentRequest(
			ctx, req, conn, conn, routerRPCSendPayment,
		)
	}

	var (
		destNode []byte
		amount   int64
		err      error
	)

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
			return fmt.Errorf("unable to decode payment amount: %w",
				err)
		}
	}

	req := &routerrpc.SendPaymentRequest{
		Dest:              destNode,
		Amt:               amount,
		DestCustomRecords: make(map[uint64][]byte),
		Amp:               ctx.Bool(ampFlag.Name),
		Cancelable:        ctx.Bool(cancelableFlag.Name),
	}

	var rHash []byte

	switch {
	case ctx.Bool("keysend") && ctx.Bool(ampFlag.Name):
		return errors.New("either keysend or amp may be set, but not both")

	case ctx.Bool("keysend"):
		if ctx.IsSet("payment_hash") {
			return errors.New("cannot set payment hash when using " +
				"keysend")
		}
		var preimage lntypes.Preimage
		if _, err := rand.Read(preimage[:]); err != nil {
			return err
		}

		// Set the preimage. If the user supplied a preimage with the
		// data flag, the preimage that is set here will be overwritten
		// later.
		req.DestCustomRecords[record.KeySendType] = preimage[:]

		hash := preimage.Hash()
		rHash = hash[:]
	case !ctx.Bool(ampFlag.Name):
		switch {
		case ctx.IsSet("payment_hash"):
			rHash, err = hex.DecodeString(ctx.String("payment_hash"))
		case args.Present():
			rHash, err = hex.DecodeString(args.First())
			args = args.Tail()
		default:
			return fmt.Errorf("payment hash argument missing")
		}
	}

	if err != nil {
		return err
	}
	if !req.Amp && len(rHash) != 32 {
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
		args = args.Tail()
		req.FinalCltvDelta = int32(delta)
	}

	payAddr, err := parsePayAddr(ctx, args)
	if err != nil {
		return err
	}

	req.PaymentAddr = payAddr

	if ctx.IsSet("route_hints") {
		// Parse the route hints JSON.
		routeHintsJSON := ctx.String("route_hints")
		var routeHints []*lnrpc.RouteHint

		err := json.Unmarshal([]byte(routeHintsJSON), &routeHints)
		if err != nil {
			return fmt.Errorf("error unmarshaling route_hints "+
				"json: %w", err)
		}

		req.RouteHints = routeHints
	}

	return SendPaymentRequest(ctx, req, conn, conn, routerRPCSendPayment)
}

// SendPaymentFn is a function type that abstracts the SendPaymentV2 call of the
// router client.
type SendPaymentFn func(ctx context.Context, payConn grpc.ClientConnInterface,
	req *routerrpc.SendPaymentRequest) (PaymentResultStream, error)

// routerRPCSendPayment is the default implementation of the SendPaymentFn type
// that uses the lnd routerrpc.SendPaymentV2 call.
func routerRPCSendPayment(ctx context.Context, payConn grpc.ClientConnInterface,
	req *routerrpc.SendPaymentRequest) (PaymentResultStream, error) {

	return routerrpc.NewRouterClient(payConn).SendPaymentV2(ctx, req)
}

func SendPaymentRequest(ctx *cli.Context, req *routerrpc.SendPaymentRequest,
	lnConn, paymentConn grpc.ClientConnInterface,
	callSendPayment SendPaymentFn) error {

	ctxc := getContext()

	lnClient := lnrpc.NewLightningClient(lnConn)

	var err error
	outChan := ctx.StringSlice("outgoing_chan_id")
	req.OutgoingChanIds, err = parseChanIDs(outChan)
	if err != nil {
		return fmt.Errorf("unable to decode outgoing_chan_ids: %w", err)
	}

	if ctx.IsSet(lastHopFlag.Name) {
		lastHop, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}
		req.LastHopPubkey = lastHop[:]
	}

	req.CltvLimit = int32(ctx.Int(cltvLimitFlag.Name))

	pmtTimeout := ctx.Duration("timeout")
	if pmtTimeout <= 0 {
		return errors.New("payment timeout must be greater than zero")
	}
	req.TimeoutSeconds = int32(pmtTimeout.Seconds())

	req.AllowSelfPayment = ctx.Bool("allow_self_payment")

	req.MaxParts = uint32(ctx.Uint(maxPartsFlag.Name))

	switch {
	// If the max shard size is specified, then it should either be in sat
	// or msat, but not both.
	case ctx.Uint64(maxShardSizeMsatFlag.Name) != 0 &&
		ctx.Uint64(maxShardSizeSatFlag.Name) != 0:
		return fmt.Errorf("only --max_split_size_msat or " +
			"--max_split_size_sat should be set, but not both")

	case ctx.Uint64(maxShardSizeMsatFlag.Name) != 0:
		req.MaxShardSizeMsat = ctx.Uint64(maxShardSizeMsatFlag.Name)

	case ctx.Uint64(maxShardSizeSatFlag.Name) != 0:
		req.MaxShardSizeMsat = uint64(lnwire.NewMSatFromSatoshis(
			btcutil.Amount(ctx.Uint64(maxShardSizeSatFlag.Name)),
		))
	}

	// Parse custom data records.
	data := ctx.String(dataFlag.Name)
	if data != "" {
		records := strings.Split(data, ",")
		for _, r := range records {
			kv := strings.Split(r, "=")
			if len(kv) != 2 {
				return errors.New("invalid data format: " +
					"multiple equal signs in record")
			}

			recordID, err := strconv.ParseUint(kv[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid data format: %w",
					err)
			}

			hexValue, err := hex.DecodeString(kv[1])
			if err != nil {
				return fmt.Errorf("invalid data format: %w",
					err)
			}

			req.DestCustomRecords[recordID] = hexValue
		}
	}

	var feeLimit int64
	if req.PaymentRequest != "" {
		// Decode payment request to find out the amount.
		decodeReq := &lnrpc.PayReqString{PayReq: req.PaymentRequest}
		decodeResp, err := lnClient.DecodePayReq(ctxc, decodeReq)
		if err != nil {
			return err
		}

		// If amount is present in the request, override the request
		// amount.
		amt := req.Amt
		invoiceAmt := decodeResp.GetNumSatoshis()
		if invoiceAmt != 0 {
			amt = invoiceAmt
		}

		// Calculate fee limit based on the determined amount.
		feeLimit, err = retrieveFeeLimit(ctx, amt)
		if err != nil {
			return err
		}

		// Ask for confirmation of amount and fee limit if payment is
		// forced.
		if !ctx.Bool("force") {
			err := confirmPayReq(decodeResp, amt, feeLimit)
			if err != nil {
				return err
			}
		}
	} else {
		var err error
		feeLimit, err = retrieveFeeLimit(ctx, req.Amt)
		if err != nil {
			return err
		}
	}

	req.FeeLimitSat = feeLimit

	// Set time pref.
	req.TimePref = ctx.Float64(timePrefFlag.Name)

	// Always print in-flight updates for the table output.
	printJSON := ctx.Bool(jsonFlag.Name)
	req.NoInflightUpdates = !ctx.Bool(inflightUpdatesFlag.Name) && printJSON

	stream, err := callSendPayment(ctxc, paymentConn, req)
	if err != nil {
		return err
	}

	finalState, err := PrintLivePayment(ctxc, stream, lnClient, printJSON)
	if err != nil {
		return err
	}

	// If we get a payment error back, we pass an error up
	// to main which eventually calls fatal() and returns
	// with a non-zero exit code.
	if finalState.Status != lnrpc.Payment_SUCCEEDED {
		return errors.New(finalState.Status.String())
	}

	return nil
}

var trackPaymentCommand = cli.Command{
	Name:     "trackpayment",
	Category: "Payments",
	Usage:    "Track progress of an existing payment.",
	Description: `
	Pick up monitoring the progression of a previously initiated payment
	specified by the hash argument.
	`,
	ArgsUsage: "hash",
	Flags: []cli.Flag{
		jsonFlag,
	},
	Action: actionDecorator(trackPayment),
}

func trackPayment(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	conn := getClientConn(ctx, false)
	defer conn.Close()

	routerClient := routerrpc.NewRouterClient(conn)

	if !args.Present() {
		return fmt.Errorf("hash argument missing")
	}

	hash, err := hex.DecodeString(args.First())
	if err != nil {
		return err
	}

	req := &routerrpc.TrackPaymentRequest{
		PaymentHash: hash,
	}

	stream, err := routerClient.TrackPaymentV2(ctxc, req)
	if err != nil {
		return err
	}

	client := lnrpc.NewLightningClient(conn)
	_, err = PrintLivePayment(ctxc, stream, client, ctx.Bool(jsonFlag.Name))
	return err
}

// PaymentResultStream is an interface that abstracts the Recv method of the
// SendPaymentV2 or TrackPaymentV2 client stream.
type PaymentResultStream interface {
	Recv() (*lnrpc.Payment, error)
}

// PrintLivePayment receives payment updates from the given stream and either
// outputs them as json or as a more user-friendly formatted table. The table
// option uses terminal control codes to rewrite the output. This call
// terminates when the payment reaches a final state.
func PrintLivePayment(ctxc context.Context, stream PaymentResultStream,
	lnClient lnrpc.LightningClient, json bool) (*lnrpc.Payment, error) {

	// Terminal escape codes aren't supported on Windows, fall back to json.
	if !json && runtime.GOOS == "windows" {
		json = true
	}

	aliases := newAliasCache(lnClient)

	first := true
	var lastLineCount int
	for {
		payment, err := stream.Recv()
		if err != nil {
			return nil, err
		}

		if json {
			// Delimit json messages by newlines (inspired by
			// grpc over rest chunking).
			if first {
				first = false
			} else {
				fmt.Println()
			}

			// Write raw json to stdout.
			printRespJSON(payment)
		} else {
			resultTable := formatPayment(ctxc, payment, aliases)

			// Clear all previously written lines and print the
			// updated table.
			clearLines(lastLineCount)
			fmt.Print(resultTable)

			// Store the number of lines written for the next update
			// pass.
			lastLineCount = 0
			for _, b := range resultTable {
				if b == '\n' {
					lastLineCount++
				}
			}
		}

		// Terminate loop if payments state is final.
		if payment.Status != lnrpc.Payment_IN_FLIGHT &&
			payment.Status != lnrpc.Payment_INITIATED {

			return payment, nil
		}
	}
}

// aliasCache allows cached retrieval of node aliases.
type aliasCache struct {
	cache  map[string]string
	client lnrpc.LightningClient
}

func newAliasCache(client lnrpc.LightningClient) *aliasCache {
	return &aliasCache{
		client: client,
		cache:  make(map[string]string),
	}
}

// get returns a node alias either from cache or freshly requested from lnd.
func (a *aliasCache) get(ctxc context.Context, pubkey string) string {
	alias, ok := a.cache[pubkey]
	if ok {
		return alias
	}

	// Request node info.
	resp, err := a.client.GetNodeInfo(
		ctxc,
		&lnrpc.NodeInfoRequest{
			PubKey: pubkey,
		},
	)
	if err != nil {
		// If no info is available, use the
		// pubkey as identifier.
		alias = pubkey[:6]
	} else {
		alias = resp.Node.Alias
	}
	a.cache[pubkey] = alias

	return alias
}

// formatMsat formats msat amounts as fractional sats.
func formatMsat(amt int64) string {
	return strconv.FormatFloat(float64(amt)/1000.0, 'f', -1, 64)
}

// formatPayment formats the payment state as an ascii table.
func formatPayment(ctxc context.Context, payment *lnrpc.Payment,
	aliases *aliasCache) string {

	t := table.NewWriter()

	// Build table header.
	t.AppendHeader(table.Row{
		"HTLC_STATE", "ATTEMPT_TIME", "RESOLVE_TIME", "RECEIVER_AMT",
		"FEE", "TIMELOCK", "CHAN_OUT", "ROUTE",
	})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Name: "ATTEMPT_TIME", Align: text.AlignRight},
		{Name: "RESOLVE_TIME", Align: text.AlignRight},
		{Name: "CHAN_OUT", Align: text.AlignLeft,
			AlignHeader: text.AlignLeft},
	})

	// Add all htlcs as rows.
	createTime := time.Unix(0, payment.CreationTimeNs)
	var totalPaid, totalFees int64
	for _, htlc := range payment.Htlcs {
		formatTime := func(timeNs int64) string {
			if timeNs == 0 {
				return "-"
			}
			resolveTime := time.Unix(0, timeNs)
			resolveTimeDiff := resolveTime.Sub(createTime)
			resolveTimeMs := resolveTimeDiff / time.Millisecond
			return fmt.Sprintf(
				"%.3f", float64(resolveTimeMs)/1000.0,
			)
		}

		attemptTime := formatTime(htlc.AttemptTimeNs)
		resolveTime := formatTime(htlc.ResolveTimeNs)

		route := htlc.Route
		lastHop := route.Hops[len(route.Hops)-1]

		hops := []string{}
		for _, h := range route.Hops {
			alias := aliases.get(ctxc, h.PubKey)
			hops = append(hops, alias)
		}

		state := htlc.Status.String()
		if htlc.Failure != nil {
			state = fmt.Sprintf(
				"%v @ %s hop",
				htlc.Failure.Code,
				ordinalNumber(htlc.Failure.FailureSourceIndex),
			)
		}

		t.AppendRow([]interface{}{
			state, attemptTime, resolveTime,
			formatMsat(lastHop.AmtToForwardMsat),
			formatMsat(route.TotalFeesMsat),
			route.TotalTimeLock, route.Hops[0].ChanId,
			strings.Join(hops, "->")},
		)

		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			totalPaid += lastHop.AmtToForwardMsat
			totalFees += route.TotalFeesMsat
		}
	}

	// Render table.
	b := &bytes.Buffer{}
	t.SetOutputMirror(b)
	t.Render()

	// Add additional payment-level data.
	fmt.Fprintf(b, "Amount + fee:   %v + %v sat\n",
		formatMsat(totalPaid), formatMsat(totalFees))
	fmt.Fprintf(b, "Payment hash:   %v\n", payment.PaymentHash)
	fmt.Fprintf(b, "Payment status: %v", payment.Status)
	switch payment.Status {
	case lnrpc.Payment_SUCCEEDED:
		fmt.Fprintf(b, ", preimage: %v", payment.PaymentPreimage)
	case lnrpc.Payment_FAILED:
		fmt.Fprintf(b, ", reason: %v", payment.FailureReason)
	}
	fmt.Fprintf(b, "\n")

	return b.String()
}

var payInvoiceCommand = cli.Command{
	Name:     "payinvoice",
	Category: "Payments",
	Usage:    "Pay an invoice over lightning.",
	Description: `
	This command is a shortcut for 'sendpayment --pay_req='.
	`,
	ArgsUsage: "pay_req",
	Flags: append(PaymentFlags(),
		cli.Int64Flag{
			Name: "amt",
			Usage: "(optional) number of satoshis to fulfill the " +
				"invoice",
		},
	),
	Action: actionDecorator(payInvoice),
}

func payInvoice(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

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

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest:    StripPrefix(payReq),
		Amt:               ctx.Int64("amt"),
		DestCustomRecords: make(map[uint64][]byte),
		Amp:               ctx.Bool(ampFlag.Name),
		Cancelable:        ctx.Bool(cancelableFlag.Name),
	}

	return SendPaymentRequest(ctx, req, conn, conn, routerRPCSendPayment)
}

var sendToRouteCommand = cli.Command{
	Name:     "sendtoroute",
	Category: "Payments",
	Usage:    "Send a payment over a predefined route.",
	Description: `
	Send a payment over Lightning using a specific route. One must specify
	the route to attempt and the payment hash. This command can even
	be chained with the response to queryroutes or buildroute. This command
	can be used to implement channel rebalancing by crafting a self-route,
	or even atomic swaps using a self-route that crosses multiple chains.

	There are three ways to specify a route:
	   * using the --routes parameter to manually specify a JSON encoded
	     route in the format of the return value of queryroutes or
	     buildroute:
	         (lncli sendtoroute --payment_hash=<pay_hash> --routes=<route>)

	   * passing the route as a positional argument:
	         (lncli sendtoroute --payment_hash=pay_hash <route>)

	   * or reading in the route from stdin, which can allow chaining the
	     response from queryroutes or buildroute, or even read in a file
	     with a pre-computed route:
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
		cli.BoolFlag{
			Name: "skip_temp_err",
			Usage: "Whether the payment should be marked as " +
				"failed when a temporary error occurred. Set " +
				"it to true so the payment won't be failed " +
				"unless a terminal error has occurred.",
		},
	},
	Action: sendToRoute,
}

func sendToRoute(ctx *cli.Context) error {
	// Show command help if no arguments provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "sendtoroute")
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
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return fmt.Errorf("queryroutes output is empty")
		}

		jsonRoutes = string(b)
	}

	// Try to parse the provided json both in the legacy QueryRoutes format
	// that contains a list of routes and the single route BuildRoute
	// format.
	var route *lnrpc.Route
	routes := &lnrpc.QueryRoutesResponse{}
	err = lnrpc.ProtoJSONUnmarshalOpts.Unmarshal([]byte(jsonRoutes), routes)
	if err == nil {
		if len(routes.Routes) == 0 {
			return fmt.Errorf("no routes provided")
		}

		if len(routes.Routes) != 1 {
			return fmt.Errorf("expected a single route, but got %v",
				len(routes.Routes))
		}

		route = routes.Routes[0]
	} else {
		routes := &routerrpc.BuildRouteResponse{}
		err = lnrpc.ProtoJSONUnmarshalOpts.Unmarshal(
			[]byte(jsonRoutes), routes,
		)
		if err != nil {
			return fmt.Errorf("unable to unmarshal json string "+
				"from incoming array of routes: %v", err)
		}

		route = routes.Route
	}

	req := &routerrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Route:       route,
		SkipTempErr: ctx.Bool("skip_temp_err"),
	}

	return sendToRouteRequest(ctx, req)
}

func sendToRouteRequest(ctx *cli.Context, req *routerrpc.SendToRouteRequest) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	resp, err := client.SendToRouteV2(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

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
			Usage: "(optional) number of blocks the last hop has " +
				"to reveal the preimage. Note that this " +
				"should not be set in the case where the " +
				"path includes a blinded path since in " +
				"that case, the receiver will already have " +
				"accounted for this value in the " +
				"blinded_cltv value",
		},
		cli.BoolFlag{
			Name:  "use_mc",
			Usage: "use mission control probabilities",
		},
		cli.StringSliceFlag{
			Name: "outgoing_chan_id",
			Usage: "(optional) the channel id of the channel " +
				"to use as the first hop. This flag can be " +
				"specified multiple times in the same command.",
		},
		cli.StringSliceFlag{
			Name: "ignore_pair",
			Usage: "ignore directional node pair " +
				"<node1>:<node2>. This flag can be specified " +
				"multiple times if multiple node pairs are " +
				"to be ignored",
		},
		timePrefFlag,
		cltvLimitFlag,
		introductionNodeFlag,
		blindingPointFlag,
		blindedHopsFlag,
		blindedBaseFlag,
		blindedPPMFlag,
		blindedCLTVFlag,
		cli.StringFlag{
			Name: "route_hints",
			Usage: `route hints for searching through private ` +
				`channels (and no blinded paths set). eg: ` +
				`'[{"hop_hints":[{"node_id":"A","chan_id":1,` +
				`"fee_base_msat":2,` +
				`"fee_proportional_millionths":3,` +
				`"cltv_expiry_delta":4}]}]'`,
		},
	},
	Action: actionDecorator(queryRoutes),
}

func queryRoutes(ctx *cli.Context) error {
	ctxc := getContext()
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

	// If we have a blinded path set, we don't have to specify a
	// destination.
	case ctx.IsSet(introductionNodeFlag.Name):

	default:
		return fmt.Errorf("dest argument missing")
	}

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %w",
				err)
		}
	default:
		return fmt.Errorf("amt argument missing")
	}

	feeLimit, err := retrieveFeeLimitLegacy(ctx)
	if err != nil {
		return err
	}

	pairs := ctx.StringSlice("ignore_pair")
	ignoredPairs := make([]*lnrpc.NodePair, len(pairs))
	for i, pair := range pairs {
		nodes := strings.Split(pair, ":")
		if len(nodes) != 2 {
			return fmt.Errorf("invalid node pair format. " +
				"Expected <node1 pub key>:<node2 pub key>")
		}

		node1, err := hex.DecodeString(nodes[0])
		if err != nil {
			return err
		}

		node2, err := hex.DecodeString(nodes[1])
		if err != nil {
			return err
		}

		ignoredPairs[i] = &lnrpc.NodePair{
			From: node1,
			To:   node2,
		}
	}

	blindedRoutes, err := parseBlindedPaymentParameters(ctx)
	if err != nil {
		return err
	}

	req := &lnrpc.QueryRoutesRequest{
		PubKey:              dest,
		Amt:                 amt,
		FeeLimit:            feeLimit,
		FinalCltvDelta:      int32(ctx.Int("final_cltv_delta")),
		UseMissionControl:   ctx.Bool("use_mc"),
		CltvLimit:           uint32(ctx.Uint64(cltvLimitFlag.Name)),
		TimePref:            ctx.Float64(timePrefFlag.Name),
		IgnoredPairs:        ignoredPairs,
		BlindedPaymentPaths: blindedRoutes,
	}

	outgoingChanIds := ctx.StringSlice("outgoing_chan_id")
	req.OutgoingChanIds, err = parseChanIDs(outgoingChanIds)
	if err != nil {
		return fmt.Errorf("unable to decode outgoing_chan_id: %w", err)
	}

	if ctx.IsSet("route_hints") {
		if len(blindedRoutes) > 0 {
			return fmt.Errorf("--route_hints should not be used " +
				"if blinded paths are set")
		}
		routeHintsJSON := ctx.String("route_hints")
		var routeHints []*lnrpc.RouteHint

		err := json.Unmarshal([]byte(routeHintsJSON), &routeHints)
		if err != nil {
			return fmt.Errorf("error unmarshaling route_hints "+
				"json: %w", err)
		}

		req.RouteHints = routeHints
	}

	route, err := client.QueryRoutes(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(route)

	return nil
}

func parseBlindedPaymentParameters(ctx *cli.Context) (
	[]*lnrpc.BlindedPaymentPath, error) {

	// Return nil if we don't have a blinding set, as we don't have a
	// blinded path.
	if !ctx.IsSet(blindingPointFlag.Name) {
		return nil, nil
	}

	// If a blinded path has been provided, then the final_cltv_delta flag
	// should not be provided since this value will be ignored.
	if ctx.IsSet("final_cltv_delta") {
		return nil, fmt.Errorf("`final_cltv_delta` should not be " +
			"provided if a blinded path is provided")
	}

	// If any one of our blinding related flags is set, we expect the
	// full set to be set and we'll error out accordingly.
	introNode, err := route.NewVertexFromStr(
		ctx.String(introductionNodeFlag.Name),
	)
	if err != nil {
		return nil, fmt.Errorf("decode introduction node: %w", err)
	}

	blindingPoint, err := route.NewVertexFromStr(ctx.String(
		blindingPointFlag.Name,
	))
	if err != nil {
		return nil, fmt.Errorf("decode blinding point: %w", err)
	}

	blindedHops := ctx.StringSlice(blindedHopsFlag.Name)

	pmt := &lnrpc.BlindedPaymentPath{
		BlindedPath: &lnrpc.BlindedPath{
			IntroductionNode: introNode[:],
			BlindingPoint:    blindingPoint[:],
			BlindedHops: make(
				[]*lnrpc.BlindedHop, len(blindedHops),
			),
		},
		BaseFeeMsat: ctx.Uint64(
			blindedBaseFlag.Name,
		),
		ProportionalFeeRate: uint32(ctx.Uint64(
			blindedPPMFlag.Name,
		)),
		TotalCltvDelta: uint32(ctx.Uint64(
			blindedCLTVFlag.Name,
		)),
	}

	for i, hop := range blindedHops {
		parts := strings.Split(hop, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("blinded hops should be "+
				"expressed as "+
				"blinded_node_id:hex_encrypted_data, got: %v",
				hop)
		}

		hop, err := route.NewVertexFromStr(parts[0])
		if err != nil {
			return nil, fmt.Errorf("hop: %v node: %w", i, err)
		}

		data, err := hex.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("hop: %v data: %w", i, err)
		}

		pmt.BlindedPath.BlindedHops[i] = &lnrpc.BlindedHop{
			BlindedNode:   hop[:],
			EncryptedData: data,
		}
	}

	return []*lnrpc.BlindedPaymentPath{
		pmt,
	}, nil
}

// retrieveFeeLimitLegacy retrieves the fee limit based on the different fee
// limit flags passed. This function will eventually disappear in favor of
// retrieveFeeLimit and the new payment rpc.
func retrieveFeeLimitLegacy(ctx *cli.Context) (*lnrpc.FeeLimit, error) {
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
		feeLimitPercent := ctx.Int64("fee_limit_percent")
		if feeLimitPercent < 0 {
			return nil, errors.New("negative fee limit percentage " +
				"provided")
		}
		return &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Percent{
				Percent: feeLimitPercent,
			},
		}, nil
	}

	// Since the fee limit flags aren't required, we don't return an error
	// if they're not set.
	return nil, nil
}

var listPaymentsCommand = cli.Command{
	Name:     "listpayments",
	Category: "Payments",
	Usage:    "List all outgoing payments.",
	Description: `
	This command enables the retrieval of payments stored
	in the database.

	Pagination is supported by the usage of index_offset in combination with
	the paginate_forwards flag.
	Reversed pagination is enabled by default to receive current payments
	first. Pagination can be resumed by using the returned last_index_offset
	(for forwards order), or first_index_offset (for reversed order) as the
	offset_index.

	Because counting all payments in the payment database can take a long
	time on systems with many payments, the count is not returned by
	default. That feature can be turned on with the --count_total_payments
	flag.
	`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "include_incomplete",
			Usage: "if set to true, payments still in flight (or " +
				"failed) will be returned as well, keeping" +
				"indices for payments the same as without " +
				"the flag",
		},
		cli.UintFlag{
			Name: "index_offset",
			Usage: "The index of a payment that will be used as " +
				"either the start (in forwards mode) or end " +
				"(in reverse mode) of a query to determine " +
				"which payments should be returned in the " +
				"response, where the index_offset is " +
				"excluded. If index_offset is set to zero in " +
				"reversed mode, the query will end with the " +
				"last payment made.",
		},
		cli.UintFlag{
			Name: "max_payments",
			Usage: "the max number of payments to return, by " +
				"default, all completed payments are returned",
		},
		cli.BoolFlag{
			Name: "paginate_forwards",
			Usage: "if set, payments succeeding the " +
				"index_offset will be returned, allowing " +
				"forwards pagination",
		},
		cli.BoolFlag{
			Name: "count_total_payments",
			Usage: "if set, all payments (complete or incomplete, " +
				"independent of max_payments parameter) will " +
				"be counted; can take a long time on systems " +
				"with many payments",
		},
		cli.Uint64Flag{
			Name: "creation_date_start",
			Usage: "timestamp in seconds, if set, filter " +
				"payments with creation date greater than or " +
				"equal to it",
		},
		cli.Uint64Flag{
			Name: "creation_date_end",
			Usage: "timestamp in seconds, if set, filter " +
				"payments with creation date less than or " +
				"equal to it",
		},
	},
	Action: actionDecorator(listPayments),
}

func listPayments(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete:  ctx.Bool("include_incomplete"),
		IndexOffset:        uint64(ctx.Uint("index_offset")),
		MaxPayments:        uint64(ctx.Uint("max_payments")),
		Reversed:           !ctx.Bool("paginate_forwards"),
		CountTotalPayments: ctx.Bool("count_total_payments"),
		CreationDateStart:  ctx.Uint64("creation_date_start"),
		CreationDateEnd:    ctx.Uint64("creation_date_end"),
	}

	payments, err := client.ListPayments(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(payments)
	return nil
}

var forwardingHistoryCommand = cli.Command{
	Name:     "fwdinghistory",
	Category: "Payments",
	Usage:    "Query the history of all forwarded HTLCs.",
	ArgsUsage: "start_time [end_time] [index_offset] [max_events]" +
		"[--incoming_channel_ids] [--outgoing_channel_ids]",
	Description: `
	Query the HTLC switch's internal forwarding log for all completed
	payment circuits (HTLCs) over a particular time range (--start_time and
	--end_time). The start and end times are meant to be expressed in
	seconds since the Unix epoch.
	Alternatively negative time ranges can be used, e.g. "-3d". Supports
	s(seconds), m(minutes), h(ours), d(ays), w(eeks), M(onths), y(ears).
	Month equals 30.44 days, year equals 365.25 days.
	If --start_time isn't provided, then 24 hours ago is used. If
	--end_time isn't provided, then the current time is used.

	The max number of events returned is 50k. The default number is 100,
	callers can use the --max_events param to modify this value.

	Incoming and outgoing channel IDs can be provided to further filter
	the events. If not provided, all events will be returned.

	Finally, callers can skip a series of events using the --index_offset
	parameter. Each response will contain the offset index of the last
	entry. Using this callers can manually paginate within a time slice.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "start_time",
			Usage: "the starting time for the query " +
				`as unix timestamp or relative e.g. "-1w"`,
		},
		cli.StringFlag{
			Name: "end_time",
			Usage: "the end time for the query " +
				`as unix timestamp or relative e.g. "-1w"`,
		},
		cli.Int64Flag{
			Name:  "index_offset",
			Usage: "the number of events to skip",
		},
		cli.Int64Flag{
			Name:  "max_events",
			Usage: "the max number of events to return",
		},
		cli.BoolFlag{
			Name: "skip_peer_alias_lookup",
			Usage: "skip the peer alias lookup per forwarding " +
				"event in order to improve performance",
		},
		cli.StringSliceFlag{
			Name: "incoming_chan_ids",
			Usage: "the short channel id of the incoming " +
				"channel to filter events by; can be " +
				"specified multiple times in the same command",
		},
		cli.StringSliceFlag{
			Name: "outgoing_chan_ids",
			Usage: "the short channel id of the outgoing " +
				"channel to filter events by; can be " +
				"specified multiple times in the same command",
		},
	},
	Action: actionDecorator(forwardingHistory),
}

func forwardingHistory(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		startTime, endTime     uint64
		indexOffset, maxEvents uint32
		err                    error
	)
	args := ctx.Args()
	now := time.Now()

	switch {
	case ctx.IsSet("start_time"):
		startTime, err = parseTime(ctx.String("start_time"), now)
	case args.Present():
		startTime, err = parseTime(args.First(), now)
		args = args.Tail()
	default:
		now := time.Now()
		startTime = uint64(now.Add(-time.Hour * 24).Unix())
	}
	if err != nil {
		return fmt.Errorf("unable to decode start_time: %w", err)
	}

	switch {
	case ctx.IsSet("end_time"):
		endTime, err = parseTime(ctx.String("end_time"), now)
	case args.Present():
		endTime, err = parseTime(args.First(), now)
		args = args.Tail()
	default:
		endTime = uint64(now.Unix())
	}
	if err != nil {
		return fmt.Errorf("unable to decode end_time: %w", err)
	}

	switch {
	case ctx.IsSet("index_offset"):
		indexOffset = uint32(ctx.Int64("index_offset"))
	case args.Present():
		i, err := strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode index_offset: %w",
				err)
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
			return fmt.Errorf("unable to decode max_events: %w",
				err)
		}
		maxEvents = uint32(m)
	}

	// By default we will look up the peers' alias information unless the
	// skip_peer_alias_lookup flag is specified.
	lookupPeerAlias := !ctx.Bool("skip_peer_alias_lookup")

	req := &lnrpc.ForwardingHistoryRequest{
		StartTime:       startTime,
		EndTime:         endTime,
		IndexOffset:     indexOffset,
		NumMaxEvents:    maxEvents,
		PeerAliasLookup: lookupPeerAlias,
	}

	outgoingChannelIDs := ctx.StringSlice("outgoing_chan_ids")
	req.OutgoingChanIds, err = parseChanIDs(outgoingChannelIDs)
	if err != nil {
		return fmt.Errorf("unable to decode outgoing_chan_ids: %w", err)
	}

	incomingChannelIDs := ctx.StringSlice("incoming_chan_ids")
	req.IncomingChanIds, err = parseChanIDs(incomingChannelIDs)
	if err != nil {
		return fmt.Errorf("unable to decode incoming_chan_ids: %w", err)
	}

	resp, err := client.ForwardingHistory(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var buildRouteCommand = cli.Command{
	Name:     "buildroute",
	Category: "Payments",
	Usage:    "Build a route from a list of hop pubkeys.",
	Description: `
	Builds a sphinx route for the supplied hops (public keys). Make sure to
	use a custom final_cltv_delta to create the route depending on the
	restrictions in the invoice otherwise LND will use its default specified
	via the bitcoin.timelockdelta setting (default 80).
	If the final_cltv_delta mismatch you will likely see the error
	INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS returned by the receiving node.

	Moreover a payment_addr has to be provided if the invoice supplied it as
	well otherwise the payment will be rejected by the receiving node.
	`,
	Action: actionDecorator(buildRoute),
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "amt",
			Usage: "the amount to send expressed in satoshis. If" +
				"not set, the minimum routable amount is used",
		},
		cli.Int64Flag{
			Name: "final_cltv_delta",
			Usage: "number of blocks the last hop has to reveal " +
				"the preimage; if not set the default lnd " +
				"final_cltv_delta is used",
		},
		cli.StringFlag{
			Name:  "hops",
			Usage: "comma separated hex pubkeys",
		},
		cli.Uint64Flag{
			Name: "outgoing_chan_id",
			Usage: "short channel id of the outgoing channel to " +
				"use for the first hop of the payment",
			Value: 0,
		},
		cli.StringFlag{
			Name: "payment_addr",
			Usage: "hex encoded payment address to set in the " +
				"last hop's mpp record",
		},
	},
}

func buildRoute(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	if !ctx.IsSet("hops") {
		return errors.New("hops required")
	}

	// Build list of hop addresses for the rpc.
	hops := strings.Split(ctx.String("hops"), ",")
	rpcHops := make([][]byte, 0, len(hops))
	for _, k := range hops {
		pubkey, err := route.NewVertexFromStr(k)
		if err != nil {
			return fmt.Errorf("error parsing %v: %w", k, err)
		}
		rpcHops = append(rpcHops, pubkey[:])
	}

	var amtMsat int64
	hasAmt := ctx.IsSet("amt")
	if hasAmt {
		amtMsat = ctx.Int64("amt") * 1000
		if amtMsat == 0 {
			return fmt.Errorf("non-zero amount required")
		}
	}

	var (
		payAddr []byte
		err     error
	)

	if ctx.IsSet("payment_addr") {
		payAddr, err = hex.DecodeString(ctx.String("payment_addr"))
		if err != nil {
			return fmt.Errorf("error parsing payment_addr: %w", err)
		}
	}

	// Call BuildRoute rpc.
	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        amtMsat,
		FinalCltvDelta: int32(ctx.Int64("final_cltv_delta")),
		HopPubkeys:     rpcHops,
		OutgoingChanId: ctx.Uint64("outgoing_chan_id"),
		PaymentAddr:    payAddr,
	}

	route, err := client.BuildRoute(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(route)

	return nil
}

var deletePaymentsCommand = cli.Command{
	Name:     "deletepayments",
	Category: "Payments",
	Usage:    "Delete a single or multiple payments from the database.",
	ArgsUsage: "--all [--failed_htlcs_only --include_non_failed] | " +
		"--payment_hash hash [--failed_htlcs_only]",
	Description: `
	This command either deletes all failed payments or a single payment from
	the database to reclaim disk space.

	If the --all flag is used, then all failed payments are removed. If so
	desired, _ALL_ payments (even the successful ones) can be deleted
	by additionally specifying --include_non_failed.

	If a --payment_hash is specified, that single payment is deleted,
	independent of its state.

	If --failed_htlcs_only is specified then the payments themselves (or the
	single payment itself if used with --payment_hash) is not deleted, only
	the information about any failed HTLC attempts during the payment.

	NOTE: Removing payments from the database does free up disk space within
	the internal bbolt database. But that disk space is only reclaimed after
	compacting the database. Users might want to turn on auto compaction
	(db.bolt.auto-compact=true in the config file or --db.bolt.auto-compact
	as a command line flag) and restart lnd after deleting a large number of
	payments to see a reduction in the file size of the channel.db file.
	`,
	Action: actionDecorator(deletePayments),
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "all",
			Usage: "delete all failed payments",
		},
		cli.StringFlag{
			Name: "payment_hash",
			Usage: "delete a specific payment identified by its " +
				"payment hash",
		},
		cli.BoolFlag{
			Name: "failed_htlcs_only",
			Usage: "only delete failed HTLCs from payments, not " +
				"the payment itself",
		},
		cli.BoolFlag{
			Name:  "include_non_failed",
			Usage: "delete ALL payments, not just the failed ones",
		},
	},
}

func deletePayments(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if arguments or no flags are provided.
	if ctx.NArg() > 0 || ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "deletepayments")
		return nil
	}

	var (
		paymentHash      []byte
		all              = ctx.Bool("all")
		singlePayment    = ctx.IsSet("payment_hash")
		failedHTLCsOnly  = ctx.Bool("failed_htlcs_only")
		includeNonFailed = ctx.Bool("include_non_failed")
		err              error
		resp             proto.Message
	)

	// We pack two RPCs into the same CLI so there are a few non-valid
	// combinations of the flags we need to filter out.
	switch {
	case all && singlePayment:
		return fmt.Errorf("cannot use --all and --payment_hash at " +
			"the same time")

	case singlePayment && includeNonFailed:
		return fmt.Errorf("cannot use --payment_hash and " +
			"--include_non_failed at the same time, when using " +
			"a payment hash the payment is deleted independent " +
			"of its state")
	}

	// Deleting a single payment is implemented in a different RPC than
	// removing all/multiple payments.
	switch {
	case singlePayment:
		paymentHash, err = hex.DecodeString(ctx.String("payment_hash"))
		if err != nil {
			return fmt.Errorf("error decoding payment_hash: %w",
				err)
		}

		resp, err = client.DeletePayment(
			ctxc, &lnrpc.DeletePaymentRequest{
				PaymentHash:     paymentHash,
				FailedHtlcsOnly: failedHTLCsOnly,
			},
		)
		if err != nil {
			return fmt.Errorf("error deleting single payment: %w",
				err)
		}

	case all:
		what := "failed"
		if includeNonFailed {
			what = "all"
		}
		if failedHTLCsOnly {
			what = fmt.Sprintf("failed HTLCs from %s", what)
		}

		fmt.Printf("Removing %s payments, this might take a while...\n",
			what)
		resp, err = client.DeleteAllPayments(
			ctxc, &lnrpc.DeleteAllPaymentsRequest{
				AllPayments:        includeNonFailed,
				FailedPaymentsOnly: !includeNonFailed,
				FailedHtlcsOnly:    failedHTLCsOnly,
			},
		)
		if err != nil {
			return fmt.Errorf("error deleting payments: %w", err)
		}

	default:
		return fmt.Errorf("either --all or --payment_hash must be set")
	}

	printJSON(resp)

	return nil
}

var estimateRouteFeeCommand = cli.Command{
	Name:     "estimateroutefee",
	Category: "Payments",
	Usage:    "Estimate routing fees based on a destination or an invoice.",
	Action:   actionDecorator(estimateRouteFee),
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest",
			Usage: "the 33-byte hex-encoded public key for the " +
				"probe destination. If it is specified then " +
				"the amt flag is required. If it isn't " +
				"specified then the pay_req field has to.",
		},
		cli.Int64Flag{
			Name: "amt",
			Usage: "the payment amount expressed in satoshis " +
				"that should be probed for. This field is " +
				"mandatory if dest is specified.",
		},
		cli.StringFlag{
			Name: "pay_req",
			Usage: "a zpay32 encoded payment request which is " +
				"used to probe. If the destination is " +
				"not public then route hints are scanned for " +
				"a public node.",
		},
		cli.DurationFlag{
			Name: "timeout",
			Usage: "a deadline for the probe attempt. Only " +
				"applicable if pay_req is specified.",
			Value: paymentTimeout,
		},
	},
}

func estimateRouteFee(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.RouteFeeRequest{}

	switch {
	case ctx.IsSet("dest") && ctx.IsSet("pay_req"):
		return fmt.Errorf("either dest or pay_req can be set")

	case ctx.IsSet("dest") && !ctx.IsSet("amt"):
		return fmt.Errorf("amt is required when dest is set")

	case ctx.IsSet("dest"):
		dest, err := hex.DecodeString(ctx.String("dest"))
		if err != nil {
			return err
		}

		if len(dest) != 33 {
			return fmt.Errorf("dest node pubkey must be exactly "+
				"33 bytes, is instead: %v", len(dest))
		}

		amtSat := ctx.Int64("amt")
		if amtSat == 0 {
			return fmt.Errorf("non-zero amount required")
		}

		req.Dest = dest
		req.AmtSat = amtSat

	case ctx.IsSet("pay_req"):
		req.PaymentRequest = StripPrefix(ctx.String("pay_req"))
		req.Timeout = uint32(ctx.Duration("timeout").Seconds())

	default:
		return fmt.Errorf("fee estimation arguments missing")
	}

	resp, err := client.EstimateRouteFee(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

// ESC is the ASCII code for escape character.
const ESC = 27

// clearCode defines a terminal escape code to clear the current line and move
// the cursor up.
var clearCode = fmt.Sprintf("%c[%dA%c[2K", ESC, 1, ESC)

// clearLines erases the last count lines in the terminal window.
func clearLines(count int) {
	_, _ = fmt.Print(strings.Repeat(clearCode, count))
}

// ordinalNumber returns the ordinal number as a string of a number.
func ordinalNumber(num uint32) string {
	switch num {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	default:
		return fmt.Sprintf("%dth", num)
	}
}

// parseChanIDs parses a slice of strings containing short channel IDs into a
// slice of uint64 values.
func parseChanIDs(idStrings []string) ([]uint64, error) {
	// Return early if no chan IDs are passed.
	if len(idStrings) == 0 {
		return nil, nil
	}

	chanIDs := make([]uint64, len(idStrings))
	for i, idStr := range idStrings {
		scid, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			return nil, err
		}

		chanIDs[i] = scid
	}

	return chanIDs, nil
}
