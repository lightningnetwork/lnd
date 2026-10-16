//go:build dev
// +build dev

package commands

import (
	"errors"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/urfave/cli"
)

// CreateOfferCommand defines the lncli createoffer command.
var CreateOfferCommand = cli.Command{
	Name:     "createoffer",
	Category: "Offers",
	Usage:    "Create a new BOLT 12 offer.",
	Description: `
	Create a new BOLT 12 offer and persist it in the offer store.
	The encoded offer string (lno1...) is returned for sharing
	with payers. The node must run with --protocol.bolt12-offers.

	Offers without an amount can be created by omitting the
	--amt_msat flag. These offers allow the payer to specify the
	amount they wish to pay.

	The expiry can be set as a duration from now with --expiry, for
	example 1h or 168h, or as a timestamp with --absolute_expiry.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "description",
			Usage: "a description of the payment purpose " +
				"to include in the offer " +
				"(required when amount is set)",
		},
		cli.Uint64Flag{
			Name: "amt_msat",
			Usage: "the amount in millisatoshis per item " +
				"(0 or omit for any-amount offers)",
		},
		cli.DurationFlag{
			Name: "expiry",
			Usage: "the duration from now after which the " +
				"offer expires, for example 1h " +
				"(cannot be used with --absolute_expiry)",
		},
		cli.Uint64Flag{
			Name: "absolute_expiry",
			Usage: "seconds since epoch after which the " +
				"offer expires (0 or omit for no " +
				"expiry)",
		},
		cli.Uint64Flag{
			Name: "quantity_max",
			Usage: "maximum number of items per invoice " +
				"(0 = unlimited, omit to disable " +
				"quantity selection)",
		},
	},
	Action: actionDecorator(createOffer),
}

func createOffer(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getDevClient(ctx)
	defer cleanUp()

	absoluteExpiry, err := offerExpiry(ctx, time.Now())
	if err != nil {
		return err
	}

	req := &devrpc.CreateOfferRequest{
		Description:    ctx.String("description"),
		AmountMsat:     ctx.Uint64("amt_msat"),
		AbsoluteExpiry: absoluteExpiry,
	}

	if ctx.IsSet("quantity_max") {
		qty := ctx.Uint64("quantity_max")
		req.QuantityMax = &qty
	}

	resp, err := client.CreateOffer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

// offerExpiry returns the absolute expiry of the offer in seconds since
// epoch, from either --expiry relative to now or --absolute_expiry. Zero
// means no expiry.
func offerExpiry(ctx *cli.Context, now time.Time) (uint64, error) {
	if !ctx.IsSet("expiry") {
		return ctx.Uint64("absolute_expiry"), nil
	}

	if ctx.IsSet("absolute_expiry") {
		return 0, errors.New("use either --expiry or " +
			"--absolute_expiry, not both")
	}

	expiry := ctx.Duration("expiry")
	if expiry <= 0 {
		return 0, errors.New("--expiry must be positive")
	}

	return uint64(now.Add(expiry).Unix()), nil
}
