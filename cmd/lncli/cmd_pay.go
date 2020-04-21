package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

const (
	// paymentTimeoutSeconds is the default timeout for the payment loop in
	// lnd. No new attempts will be started after the timeout.
	paymentTimeoutSeconds = 60
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

	showInflightFlag = cli.BoolFlag{
		Name: "show_inflight",
		Usage: "if set, intermediate payment state updates will be " +
			"displayed",
	}

	maxShardsFlag = cli.UintFlag{
		Name: "max_shards",
		Usage: "the maximum number of partial payments that may be " +
			"used",
		Value: 1,
	}
)

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
		lastHopFlag,
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
		cli.BoolFlag{
			Name:  "allow_self_payment",
			Usage: "allow sending a circular payment to self",
		},
		dataFlag, showInflightFlag, maxShardsFlag,
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
		cli.Int64Flag{
			Name:  "final_cltv_delta",
			Usage: "the number of blocks the last hop has to reveal the preimage",
		},
		cli.BoolFlag{
			Name:  "keysend",
			Usage: "will generate a pre-image and encode it in the sphinx packet, a dest must be set [experimental]",
		},
	),
	Action: sendPayment,
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

	// If no fee limit is set, use the payment amount as a limit (100%).
	return amt, nil
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

func sendPayment(ctx *cli.Context) error {
	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "sendpayment")
		return nil
	}

	// If a payment request was provided, we can exit early since all of the
	// details of the payment are encoded within the request.
	if ctx.IsSet("pay_req") {
		req := &routerrpc.SendPaymentRequest{
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

	req := &routerrpc.SendPaymentRequest{
		Dest:              destNode,
		Amt:               amount,
		DestCustomRecords: make(map[uint64][]byte),
	}

	var rHash []byte

	if ctx.Bool("keysend") {
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
	} else {
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

	return sendPaymentRequest(ctx, req)
}

func sendPaymentRequest(ctx *cli.Context,
	req *routerrpc.SendPaymentRequest) error {

	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := lnrpc.NewLightningClient(conn)
	routerClient := routerrpc.NewRouterClient(conn)

	req.OutgoingChanId = ctx.Uint64("outgoing_chan_id")
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
	req.TimeoutSeconds = paymentTimeoutSeconds

	req.AllowSelfPayment = ctx.Bool("allow_self_payment")

	req.MaxShards = uint32(ctx.Uint(maxShardsFlag.Name))

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
				return fmt.Errorf("invalid data format: %v",
					err)
			}

			hexValue, err := hex.DecodeString(kv[1])
			if err != nil {
				return fmt.Errorf("invalid data format: %v",
					err)
			}

			req.DestCustomRecords[recordID] = hexValue
		}
	}

	var feeLimit int64
	if req.PaymentRequest != "" {
		// Decode payment request to find out the amount.
		decodeReq := &lnrpc.PayReqString{PayReq: req.PaymentRequest}
		decodeResp, err := client.DecodePayReq(
			context.Background(), decodeReq,
		)
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

	showInflight := ctx.Bool(showInflightFlag.Name)

	stream, err := routerClient.SendPaymentV2(context.Background(), req)
	if err != nil {
		return err
	}

	for {
		status, err := stream.Recv()
		if err != nil {
			return err
		}

		if status.Status != lnrpc.Payment_IN_FLIGHT {
			printRespJSON(status)

			// If we get a payment error back, we pass an error up
			// to main which eventually calls fatal() and returns
			// with a non-zero exit code.
			if status.Status != lnrpc.Payment_SUCCEEDED {
				return errors.New(status.Status.String())
			}

			return nil
		}

		if showInflight {
			printRespJSON(status)
		}
	}
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
	Action:    actionDecorator(trackPayment),
}

func trackPayment(ctx *cli.Context) error {
	args := ctx.Args()

	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

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

	stream, err := client.TrackPaymentV2(context.Background(), req)
	if err != nil {
		return err
	}

	for {
		status, err := stream.Recv()
		if err != nil {
			return err
		}

		printRespJSON(status)

		if status.Status != lnrpc.Payment_IN_FLIGHT {
			return nil
		}
	}
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

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest:    payReq,
		Amt:               ctx.Int64("amt"),
		DestCustomRecords: make(map[uint64][]byte),
	}

	return sendPaymentRequest(ctx, req)
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
		b, err := ioutil.ReadAll(os.Stdin)
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
	err = jsonpb.UnmarshalString(jsonRoutes, routes)
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
		err = jsonpb.UnmarshalString(jsonRoutes, routes)
		if err != nil {
			return fmt.Errorf("unable to unmarshal json string "+
				"from incoming array of routes: %v", err)
		}

		route = routes.Route
	}

	req := &lnrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Route:       route,
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

	printRespJSON(resp)

	return nil
}
