//go:build invoicesrpc
// +build invoicesrpc

package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/urfave/cli/v3"
)

// invoicesCommands will return nil for non-invoicesrpc builds.
func invoicesCommands() []*cli.Command {
	return []*cli.Command{
		cancelInvoiceCommand,
		addHoldInvoiceCommand,
		settleInvoiceCommand,
	}
}

func getInvoicesClient(cmd *cli.Command) (invoicesrpc.InvoicesClient, func()) {
	conn := getClientConn(cmd, false)

	cleanUp := func() {
		conn.Close()
	}

	return invoicesrpc.NewInvoicesClient(conn), cleanUp
}

var settleInvoiceCommand = &cli.Command{
	Name:     "settleinvoice",
	Category: "Invoices",
	Usage:    "Reveal a preimage and use it to settle the corresponding invoice.",
	Description: `
	Todo.`,
	ArgsUsage: "preimage",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "preimage",
			Usage: "the hex-encoded preimage (32 byte) which will " +
				"allow settling an incoming HTLC payable to this " +
				"preimage.",
		},
	},
	Action: actionDecorator(settleInvoice),
}

func settleInvoice(ctx context.Context, cmd *cli.Command) error {
	var (
		preimage []byte
		err      error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(cmd)
	defer cleanUp()

	args := cmd.Args().Slice()

	switch {
	case cmd.IsSet("preimage"):
		preimage, err = hex.DecodeString(cmd.String("preimage"))
	case len(args) > 0:
		preimage, err = hex.DecodeString(args[0])
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

var cancelInvoiceCommand = &cli.Command{
	Name:     "cancelinvoice",
	Category: "Invoices",
	Usage:    "Cancels a (hold) invoice.",
	Description: `
	Todo.`,
	ArgsUsage: "paymenthash",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "paymenthash",
			Usage: "the hex-encoded payment hash (32 byte) for which the " +
				"corresponding invoice will be canceled.",
		},
	},
	Action: actionDecorator(cancelInvoice),
}

func cancelInvoice(ctx context.Context, cmd *cli.Command) error {
	var (
		paymentHash []byte
		err         error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(cmd)
	defer cleanUp()

	args := cmd.Args().Slice()

	switch {
	case cmd.IsSet("paymenthash"):
		paymentHash, err = hex.DecodeString(cmd.String("paymenthash"))
	case len(args) > 0:
		paymentHash, err = hex.DecodeString(args[0])
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

var addHoldInvoiceCommand = &cli.Command{
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
		&cli.StringFlag{
			Name: "memo",
			Usage: "a description of the payment to attach along " +
				"with the invoice (default=\"\")",
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
				"payer in reaching you",
		},
	},
	Action: actionDecorator(addHoldInvoice),
}

func addHoldInvoice(ctx context.Context, cmd *cli.Command) error {
	var (
		descHash []byte
		err      error
	)

	ctxc := getContext()
	client, cleanUp := getInvoicesClient(cmd)
	defer cleanUp()

	args := cmd.Args().Slice()
	if cmd.NArg() == 0 {
		cli.ShowCommandHelp(ctx, cmd, "addholdinvoice")
		return nil
	}

	hash, err := hex.DecodeString(args[0])
	if err != nil {
		return fmt.Errorf("unable to parse hash: %w", err)
	}

	args = args[1:]

	amt := cmd.Int("amt")
	amtMsat := cmd.Int("amt_msat")

	if !cmd.IsSet("amt") && !cmd.IsSet("amt_msat") && len(args) > 0 {
		amt, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %w",
				err)
		}
	}

	descHash, err = hex.DecodeString(cmd.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %w", err)
	}

	invoice := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:            cmd.String("memo"),
		Hash:            hash,
		Value:           amt,
		ValueMsat:       amtMsat,
		DescriptionHash: descHash,
		FallbackAddr:    cmd.String("fallback_addr"),
		Expiry:          cmd.Int("expiry"),
		CltvExpiry:      cmd.Uint("cltv_expiry_delta"),
		Private:         cmd.Bool("private"),
	}

	resp, err := client.AddHoldInvoice(ctxc, invoice)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
