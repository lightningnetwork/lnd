package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

// newHopHint creates a new lnrpc.HopHint from the provided parameters.
func newHopHint(nodeID string, chanID uint64, feeBaseMsat uint32,
	feeProportionalMillionths uint32, cltvExpiryDelta uint32) (
	*lnrpc.HopHint, error,
) {

	if nodeID == "" {
		return nil, fmt.Errorf("node_id is missing in route hint")
	}

	if chanID == 0 {
		return nil, fmt.Errorf("chan_id is missing or invalid in " +
			"route hint")
	}

	return &lnrpc.HopHint{
		NodeId:                    nodeID,
		ChanId:                    chanID,
		FeeBaseMsat:               feeBaseMsat,
		FeeProportionalMillionths: feeProportionalMillionths,
		CltvExpiryDelta:           cltvExpiryDelta,
	}, nil
}

// parseRouteHints parses the route hints from a JSON string and returns a
// slice of lnrpc.HopHint.
func parseRouteHints(routeHints string) ([]*lnrpc.HopHint, error) {
	var jsonHints []*lnrpc.HopHint
	err := json.Unmarshal([]byte(routeHints), &jsonHints)

	if err != nil {
		return nil, fmt.Errorf("error parsing route hints JSON: "+
			"%w", err)
	}

	if len(jsonHints) == 0 {
		return nil, fmt.Errorf("no valid route hints found in JSON")
	}

	var invoiceHints []*lnrpc.HopHint
	for _, jsonHint := range jsonHints {
		newHint, err := newHopHint(
			jsonHint.NodeId,
			jsonHint.ChanId,
			jsonHint.FeeBaseMsat,
			jsonHint.FeeProportionalMillionths,
			jsonHint.CltvExpiryDelta,
		)

		if err != nil {
			return nil, err
		}

		invoiceHints = append(invoiceHints, newHint)
	}

	return invoiceHints, nil
}

var addInvoiceCommand = cli.Command{
	Name:     "addinvoice",
	Category: "Invoices",
	Usage:    "Add a new invoice.",
	Description: `
	Add a new invoice, expressing intent for a future payment.

	Invoices without an amount can be created by not supplying any
	parameters or providing an amount of 0. These invoices allow the payer
	to specify the amount of satoshis they wish to send.`,
	ArgsUsage: "value preimage",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "memo",
			Usage: "a description of the payment to attach along " +
				"with the invoice (default=\"\")",
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
		cli.Int64Flag{
			Name:  "amt_msat",
			Usage: "the amt of millisatoshis in this invoice",
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
				"specified, an expiry of " +
				"86400 seconds (24 hours) is implied.",
		},
		cli.Uint64Flag{
			Name: "cltv_expiry_delta",
			Usage: "The minimum CLTV delta to use for the final " +
				"hop. If this is set to 0, the default value " +
				"is used. The default value for " +
				"cltv_expiry_delta is configured by the " +
				"'bitcoin.timelockdelta' option.",
		},
		cli.BoolFlag{
			Name: "private",
			Usage: "encode routing hints in the invoice with " +
				"private channels in order to assist the " +
				"payer in reaching you. If amt and amt_msat " +
				"are zero, a large number of hints with " +
				"these channels can be included, which " +
				"might not be desirable.",
		},
		cli.BoolFlag{
			Name: "amp",
			Usage: "creates an AMP invoice. If true, preimage " +
				"should not be set.",
		},
		cli.StringFlag{
			Name: "route_hints",
			Usage: "A string representing route " +
				"hints for the invoice. Each route hint " +
				"should be a JSON object with fields: " +
				"node_id (string), chan_id (number), " +
				"fee_base_msat (number, optional), " +
				"fee_proportional_millionths " +
				"(number, optional), cltv_expiry_delta " +
				"(number, optional). Example: " +
				`'[{"node_id":"02b2a...",` +
				`"chan_id":221001837248512,` +
				`"fee_base_msat":1000,` +
				`"fee_proportional_millionths":100,` +
				`"cltv_expiry_delta":40}]'`,
		},
	},
	Action: actionDecorator(addInvoice),
}

func addInvoice(ctx *cli.Context) error {
	var (
		preimage []byte
		descHash []byte
		amt      int64
		amtMsat  int64
		err      error
	)
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	amt = ctx.Int64("amt")
	amtMsat = ctx.Int64("amt_msat")
	if !ctx.IsSet("amt") && !ctx.IsSet("amt_msat") && args.Present() {
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		args = args.Tail()
		if err != nil {
			return fmt.Errorf(
				"unable to decode amt argument: %w", err,
			)
		}
	}

	switch {
	case ctx.IsSet("preimage"):
		preimage, err = hex.DecodeString(ctx.String("preimage"))
		// If preimage is not set, but the argument is present, we'll
		// assume the user has passed in the preimage as a positional
		// argument.
	case args.Present() && len(args.First()) == 64:
		preimage, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %w", err)
	}

	descHash, err = hex.DecodeString(ctx.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %w", err)
	}

	routeHintsJSON := ctx.String("route_hints")
	var invoiceHints []*lnrpc.HopHint
	if routeHintsJSON != "" {
		if !ctx.Bool("private") {
			return fmt.Errorf(
				"route hints can only be included if the " +
					"invoice is marked as private",
			)
		}

		// Parse the hop hints from the JSON string.
		invoiceHints, err = parseRouteHints(routeHintsJSON)
		if err != nil {
			return err
		}
	}
	routeHints := make([]*lnrpc.RouteHint, 0, len(invoiceHints))
	for _, hint := range invoiceHints {
		// Add each invoice hint to the route hints.
		routeHints = append(routeHints, &lnrpc.RouteHint{
			HopHints: []*lnrpc.HopHint{hint},
		})
	}

	invoice := &lnrpc.Invoice{
		Memo:            ctx.String("memo"),
		RPreimage:       preimage,
		Value:           amt,
		ValueMsat:       amtMsat,
		DescriptionHash: descHash,
		FallbackAddr:    ctx.String("fallback_addr"),
		Expiry:          ctx.Int64("expiry"),
		CltvExpiry:      ctx.Uint64("cltv_expiry_delta"),
		Private:         ctx.Bool("private"),
		IsAmp:           ctx.Bool("amp"),
		RouteHints:      routeHints,
	}

	resp, err := client.AddInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var lookupInvoiceCommand = cli.Command{
	Name:      "lookupinvoice",
	Category:  "Invoices",
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
	ctxc := getContext()
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
		return fmt.Errorf("unable to decode rhash argument: %w", err)
	}

	req := &lnrpc.PaymentHash{
		RHash: rHash,
	}

	invoice, err := client.LookupInvoice(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(invoice)

	return nil
}

var listInvoicesCommand = cli.Command{
	Name:     "listinvoices",
	Category: "Invoices",
	Usage: "List all invoices currently stored within the database. Any " +
		"active debug invoices are ignored.",
	Description: `
	This command enables the retrieval of all invoices currently stored
	within the database. It has full support for paginationed responses,
	allowing users to query for specific invoices through their add_index.
	This can be done by using either the first_index_offset or
	last_index_offset fields included in the response as the index_offset of
	the next request. Backward pagination is enabled by default to receive
	current invoices first. If you wish to paginate forwards, set the 
	paginate-forwards flag.	If none of the parameters are specified, then 
	the last 100 invoices will be returned.

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
		cli.BoolFlag{
			Name: "paginate-forwards",
			Usage: "if set, invoices succeeding the " +
				"index_offset will be returned",
		},
		cli.Uint64Flag{
			Name: "creation_date_start",
			Usage: "timestamp in seconds, if set, filter " +
				"invoices with creation date greater than or " +
				"equal to it",
		},
		cli.Uint64Flag{
			Name: "creation_date_end",
			Usage: "timestamp in seconds, if set, filter " +
				"invoices with creation date less than or " +
				"equal to it",
		},
	},
	Action: actionDecorator(listInvoices),
}

func listInvoices(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListInvoiceRequest{
		PendingOnly:       ctx.Bool("pending_only"),
		IndexOffset:       ctx.Uint64("index_offset"),
		NumMaxInvoices:    ctx.Uint64("max_invoices"),
		Reversed:          !ctx.Bool("paginate-forwards"),
		CreationDateStart: ctx.Uint64("creation_date_start"),
		CreationDateEnd:   ctx.Uint64("creation_date_end"),
	}

	invoices, err := client.ListInvoices(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(invoices)

	return nil
}

var decodePayReqCommand = cli.Command{
	Name:        "decodepayreq",
	Category:    "Invoices",
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
	ctxc := getContext()
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

	resp, err := client.DecodePayReq(ctxc, &lnrpc.PayReqString{
		PayReq: stripPrefix(payreq),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
