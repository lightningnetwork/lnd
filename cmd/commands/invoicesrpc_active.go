//go:build invoicesrpc
// +build invoicesrpc

package commands

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/urfave/cli"
)

// invoicesCommands will return nil for non-invoicesrpc builds.
func invoicesCommands() []cli.Command {
	return []cli.Command{
		cancelInvoiceCommand,
		addHoldInvoiceCommand,
		settleInvoiceCommand,
	}
}

func getInvoicesClient(ctx *cli.Context) (invoicesrpc.InvoicesClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return invoicesrpc.NewInvoicesClient(conn), cleanUp
}

var settleInvoiceCommand = cli.Command{
	Name:     "settleinvoice",
	Category: "Invoices",
	Usage:    "Reveal a preimage and use it to settle the corresponding invoice.",
	Description: `
	Todo.`,
	ArgsUsage: "preimage",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "preimage",
			Usage: "the hex-encoded preimage (32 byte) which will " +
				"allow settling an incoming HTLC payable to this " +
				"preimage.",
		},
	},
	Action: actionDecorator(settleInvoice),
}

func settleInvoice(ctx *cli.Context) error {
	var (
		preimage []byte
		err      error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	switch {
	case ctx.IsSet("preimage"):
		preimage, err = hex.DecodeString(ctx.String("preimage"))
	case args.Present():
		preimage, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %w", err)
	}

	invoice := &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage,
	}

	resp, err := client.SettleInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var cancelInvoiceCommand = cli.Command{
	Name:     "cancelinvoice",
	Category: "Invoices",
	Usage:    "Cancels a (hold) invoice.",
	Description: `
	Todo.`,
	ArgsUsage: "paymenthash",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "paymenthash",
			Usage: "the hex-encoded payment hash (32 byte) for which the " +
				"corresponding invoice will be canceled.",
		},
	},
	Action: actionDecorator(cancelInvoice),
}

func cancelInvoice(ctx *cli.Context) error {
	var (
		paymentHash []byte
		err         error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	switch {
	case ctx.IsSet("paymenthash"):
		paymentHash, err = hex.DecodeString(ctx.String("paymenthash"))
	case args.Present():
		paymentHash, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %w", err)
	}

	invoice := &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: paymentHash,
	}

	resp, err := client.CancelInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var addHoldInvoiceCommand = cli.Command{
	Name:     "addholdinvoice",
	Category: "Invoices",
	Usage:    "Add a new hold invoice.",
	Description: `
	Add a new invoice, expressing intent for a future payment.

	Invoices without an amount can be created by not supplying any
	parameters or providing an amount of 0. These invoices allow the payer
	to specify the amount of satoshis they wish to send.`,
	ArgsUsage: "hash [amt]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "memo",
			Usage: "a description of the payment to attach along " +
				"with the invoice (default=\"\")",
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
				"payer in reaching you",
		},
	},
	Action: actionDecorator(addHoldInvoice),
}

func addHoldInvoice(ctx *cli.Context) error {
	var (
		descHash []byte
		err      error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(ctx)
	defer cleanUp()

	args := ctx.Args()
	if ctx.NArg() == 0 {
		cli.ShowCommandHelp(ctx, "addholdinvoice")
		return nil
	}

	hash, err := hex.DecodeString(args.First())
	if err != nil {
		return fmt.Errorf("unable to parse hash: %w", err)
	}

	args = args.Tail()

	amt := ctx.Int64("amt")
	amtMsat := ctx.Int64("amt_msat")

	if !ctx.IsSet("amt") && !ctx.IsSet("amt_msat") && args.Present() {
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %w",
				err)
		}
	}

	descHash, err = hex.DecodeString(ctx.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %w", err)
	}

	invoice := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:            ctx.String("memo"),
		Hash:            hash,
		Value:           amt,
		ValueMsat:       amtMsat,
		DescriptionHash: descHash,
		FallbackAddr:    ctx.String("fallback_addr"),
		Expiry:          ctx.Int64("expiry"),
		CltvExpiry:      ctx.Uint64("cltv_expiry_delta"),
		Private:         ctx.Bool("private"),
	}

	resp, err := client.AddHoldInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
