package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli/v3"
)

var AddInvoiceCommand = &cli.Command{
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
		&cli.StringFlag{
			Name: "memo",
			Usage: "a description of the payment to attach along " +
				"with the invoice (default=\"\")",
		},
		&cli.StringFlag{
			Name: "preimage",
			Usage: "the hex-encoded preimage (32 byte) which will " +
				"allow settling an incoming HTLC payable to this " +
				"preimage. If not set, a random preimage will be " +
				"created.",
		},
		&cli.IntFlag{
			Name:  "amt",
			Usage: "the amt of satoshis in this invoice",
		},
		&cli.IntFlag{
			Name:  "amt_msat",
			Usage: "the amt of millisatoshis in this invoice",
		},
		&cli.StringFlag{
			Name: "description_hash",
			Usage: "SHA-256 hash of the description of the payment. " +
				"Used if the purpose of payment cannot naturally " +
				"fit within the memo. If provided this will be " +
				"used instead of the description(memo) field in " +
				"the encoded invoice.",
		},
		&cli.StringFlag{
			Name: "fallback_addr",
			Usage: "fallback on-chain address that can be used in " +
				"case the lightning payment fails",
		},
		&cli.IntFlag{
			Name: "expiry",
			Usage: "the invoice's expiry time in seconds. If not " +
				"specified, an expiry of " +
				"86400 seconds (24 hours) is implied.",
		},
		&cli.UintFlag{
			Name: "cltv_expiry_delta",
			Usage: "The minimum CLTV delta to use for the final " +
				"hop. If this is set to 0, the default value " +
				"is used. The default value for " +
				"cltv_expiry_delta is configured by the " +
				"'bitcoin.timelockdelta' option.",
		},
		&cli.BoolFlag{
			Name: "private",
			Usage: "encode routing hints in the invoice with " +
				"private channels in order to assist the " +
				"payer in reaching you. If amt and amt_msat " +
				"are zero, a large number of hints with " +
				"these channels can be included, which " +
				"might not be desirable.",
		},
		&cli.BoolFlag{
			Name: "amp",
			Usage: "creates an AMP invoice. If true, preimage " +
				"should not be set.",
		},
		&cli.BoolFlag{
			Name: "blind",
			Usage: "creates an invoice that contains blinded " +
				"paths. Note that invoices with blinded " +
				"paths will be signed using a random " +
				"ephemeral key so as not to reveal the real " +
				"node ID of this node.",
		},
		&cli.UintFlag{
			Name: "min_real_blinded_hops",
			Usage: "The minimum number of real hops to use in a " +
				"blinded path. This option will only be used " +
				"if `--blind` has also been set.",
		},
		&cli.UintFlag{
			Name: "num_blinded_hops",
			Usage: "The number of hops to use for each " +
				"blinded path included in the invoice. This " +
				"option will only be used if `--blind` has " +
				"also been set. Dummy hops will be used to " +
				"pad paths shorter than this.",
		},
		&cli.UintFlag{
			Name: "max_blinded_paths",
			Usage: "The maximum number of blinded paths to add " +
				"to an invoice. This option will only be " +
				"used if `--blind` has also been set.",
		},
		&cli.StringSliceFlag{
			Name: "blinded_path_omit_node",
			Usage: "The pub key (in hex) of a node not to " +
				"use on a blinded path. The flag may be " +
				"specified multiple times.",
		},
	},
	Action: actionDecorator(addInvoice),
}

func addInvoice(ctx context.Context, cmd *cli.Command) error {
	var (
		preimage []byte
		descHash []byte
		amt      int64
		amtMsat  int64
		err      error
	)
	ctxc := getContext()
	client, cleanUp := getClient(cmd)
	defer cleanUp()

	args := cmd.Args().Slice()

	amt = cmd.Int("amt")
	amtMsat = cmd.Int("amt_msat")
	if !cmd.IsSet("amt") && !cmd.IsSet("amt_msat") && len(args) > 0 {
		amt, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %w",
				err)
		}
	}

	switch {
	case cmd.IsSet("preimage"):
		preimage, err = hex.DecodeString(cmd.String("preimage"))
	case len(args[1:]) > 0:
		preimage, err = hex.DecodeString(args[1:][0])
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %w", err)
	}

	descHash, err = hex.DecodeString(cmd.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %w", err)
	}

	if cmd.IsSet("private") && cmd.IsSet("blind") {
		return fmt.Errorf("cannot include both route hints and " +
			"blinded paths in the same invoice")
	}

	blindedPathCfg, err := parseBlindedPathCfg(cmd)
	if err != nil {
		return fmt.Errorf("could not parse blinded path config: %w",
			err)
	}

	invoice := &lnrpc.Invoice{
		Memo:              cmd.String("memo"),
		RPreimage:         preimage,
		Value:             amt,
		ValueMsat:         amtMsat,
		DescriptionHash:   descHash,
		FallbackAddr:      cmd.String("fallback_addr"),
		Expiry:            cmd.Int("expiry"),
		CltvExpiry:        cmd.Uint("cltv_expiry_delta"),
		Private:           cmd.Bool("private"),
		IsAmp:             cmd.Bool("amp"),
		IsBlinded:         cmd.Bool("blind"),
		BlindedPathConfig: blindedPathCfg,
	}

	resp, err := client.AddInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func parseBlindedPathCfg(cmd *cli.Command) (*lnrpc.BlindedPathConfig, error) {
	if !cmd.Bool("blind") {
		if cmd.IsSet("min_real_blinded_hops") ||
			cmd.IsSet("num_blinded_hops") ||
			cmd.IsSet("max_blinded_paths") ||
			cmd.IsSet("blinded_path_omit_node") {

			return nil, fmt.Errorf("blinded path options are " +
				"only used if the `--blind` options is set")
		}

		return nil, nil
	}

	var blindCfg lnrpc.BlindedPathConfig

	if cmd.IsSet("min_real_blinded_hops") {
		minNumRealHops := uint32(cmd.Uint("min_real_blinded_hops"))
		blindCfg.MinNumRealHops = &minNumRealHops
	}

	if cmd.IsSet("num_blinded_hops") {
		numHops := uint32(cmd.Uint("num_blinded_hops"))
		blindCfg.NumHops = &numHops
	}

	if cmd.IsSet("max_blinded_paths") {
		maxPaths := uint32(cmd.Uint("max_blinded_paths"))
		blindCfg.MaxNumPaths = &maxPaths
	}

	for _, pubKey := range cmd.StringSlice("blinded_path_omit_node") {
		pubKeyBytes, err := hex.DecodeString(pubKey)
		if err != nil {
			return nil, err
		}

		blindCfg.NodeOmissionList = append(
			blindCfg.NodeOmissionList, pubKeyBytes,
		)
	}

	return &blindCfg, nil
}

var lookupInvoiceCommand = &cli.Command{
	Name:      "lookupinvoice",
	Category:  "Invoices",
	Usage:     "Lookup an existing invoice by its payment hash.",
	ArgsUsage: "rhash",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "rhash",
			Usage: "the 32 byte payment hash of the invoice to query for, the hash " +
				"should be a hex-encoded string",
		},
	},
	Action: actionDecorator(lookupInvoice),
}

func lookupInvoice(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getClient(cmd)
	defer cleanUp()

	var (
		rHash []byte
		err   error
	)

	switch {
	case cmd.IsSet("rhash"):
		rHash, err = hex.DecodeString(cmd.String("rhash"))
	case cmd.Args().Present():
		rHash, err = hex.DecodeString(cmd.Args().First())
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

var listInvoicesCommand = &cli.Command{
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
		&cli.BoolFlag{
			Name: "pending_only",
			Usage: "toggles if all invoices should be returned, " +
				"or only those that are currently unsettled",
		},
		&cli.UintFlag{
			Name: "index_offset",
			Usage: "the index of an invoice that will be used as " +
				"either the start or end of a query to " +
				"determine which invoices should be returned " +
				"in the response",
		},
		&cli.UintFlag{
			Name:  "max_invoices",
			Usage: "the max number of invoices to return",
		},
		&cli.BoolFlag{
			Name: "paginate-forwards",
			Usage: "if set, invoices succeeding the " +
				"index_offset will be returned",
		},
		&cli.UintFlag{
			Name: "creation_date_start",
			Usage: "timestamp in seconds, if set, filter " +
				"invoices with creation date greater than or " +
				"equal to it",
		},
		&cli.UintFlag{
			Name: "creation_date_end",
			Usage: "timestamp in seconds, if set, filter " +
				"invoices with creation date less than or " +
				"equal to it",
		},
	},
	Action: actionDecorator(listInvoices),
}

func listInvoices(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getClient(cmd)
	defer cleanUp()

	req := &lnrpc.ListInvoiceRequest{
		PendingOnly:       cmd.Bool("pending_only"),
		IndexOffset:       cmd.Uint("index_offset"),
		NumMaxInvoices:    cmd.Uint("max_invoices"),
		Reversed:          !cmd.Bool("paginate-forwards"),
		CreationDateStart: cmd.Uint("creation_date_start"),
		CreationDateEnd:   cmd.Uint("creation_date_end"),
	}

	invoices, err := client.ListInvoices(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(invoices)

	return nil
}

var decodePayReqCommand = &cli.Command{
	Name:        "decodepayreq",
	Category:    "Invoices",
	Usage:       "Decode a payment request.",
	Description: "Decode the passed payment request revealing the destination, payment hash and value of the payment request",
	ArgsUsage:   "pay_req",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "pay_req",
			Usage: "the bech32 encoded payment request",
		},
	},
	Action: actionDecorator(decodePayReq),
}

func decodePayReq(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getClient(cmd)
	defer cleanUp()

	var payreq string

	switch {
	case cmd.IsSet("pay_req"):
		payreq = cmd.String("pay_req")
	case cmd.Args().Present():
		payreq = cmd.Args().Slice()[0]
	default:
		return fmt.Errorf("pay_req argument missing")
	}

	resp, err := client.DecodePayReq(ctxc, &lnrpc.PayReqString{
		PayReq: StripPrefix(payreq),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
