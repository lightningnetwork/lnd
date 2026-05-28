package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var rebalanceCommand = cli.Command{
	Name:     "rebalance",
	Category: "Channels",
	Usage:    "Move liquidity from one channel to another via a circular self-payment.",
	Description: `
rebalance shifts local liquidity from --out_chan to --in_chan by routing a
circular self-payment: the payment leaves through the outgoing channel, hops
across the network, and re-enters via the last hop peer of the incoming channel.

Both channels must be active and the outgoing channel must have at least
--amount sats of local balance.

Example:
  lncli rebalance --out_chan 123456789 --in_chan 987654321 --amount 500000`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "out_chan",
			Usage: "channel ID to drain (source)",
		},
		cli.Uint64Flag{
			Name:  "in_chan",
			Usage: "channel ID to fill (destination)",
		},
		cli.Int64Flag{
			Name:  "amount",
			Usage: "satoshis to move",
		},
		cli.Int64Flag{
			Name:  "max_fee_sat",
			Value: 0,
			Usage: "maximum routing fee in sats (0 = no limit)",
		},
		cli.IntFlag{
			Name:  "timeout",
			Value: 60,
			Usage: "seconds to wait for the payment to complete",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print result as JSON",
		},
	},
	Action: actionDecorator(rebalance),
}

func rebalance(ctx *cli.Context) error {
	// Validate required flags.
	outChanID := ctx.Uint64("out_chan")
	inChanID := ctx.Uint64("in_chan")
	amount := ctx.Int64("amount")

	if outChanID == 0 {
		return fmt.Errorf("--out_chan is required")
	}
	if inChanID == 0 {
		return fmt.Errorf("--in_chan is required")
	}
	if amount <= 0 {
		return fmt.Errorf("--amount must be a positive number of sats")
	}
	if outChanID == inChanID {
		return fmt.Errorf("--out_chan and --in_chan must be different channels")
	}

	timeoutSecs := ctx.Int("timeout")
	ctxc, cancel := context.WithTimeout(getContext(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	conn := getClientConn(ctx, false)
	defer conn.Close()

	lnClient := lnrpc.NewLightningClient(conn)

	// Look up both channels to validate and get the in_chan peer pubkey.
	listResp, err := lnClient.ListChannels(ctxc, &lnrpc.ListChannelsRequest{
		ActiveOnly: false,
	})
	if err != nil {
		return fmt.Errorf("ListChannels failed: %w", err)
	}

	var outChan, inChan *lnrpc.Channel
	for _, ch := range listResp.Channels {
		ch := ch
		switch ch.ChanId {
		case outChanID:
			outChan = ch
		case inChanID:
			inChan = ch
		}
	}

	if outChan == nil {
		return fmt.Errorf("outgoing channel %d not found", outChanID)
	}
	if inChan == nil {
		return fmt.Errorf("incoming channel %d not found", inChanID)
	}
	if !outChan.Active {
		return fmt.Errorf("outgoing channel %d is not active", outChanID)
	}
	if !inChan.Active {
		return fmt.Errorf("incoming channel %d is not active", inChanID)
	}
	if outChan.LocalBalance < amount {
		return fmt.Errorf("outgoing channel has only %s local balance, need %s",
			commaSats(outChan.LocalBalance), commaSats(amount))
	}

	// Get our own pubkey so we can use it as the payment destination.
	infoResp, err := lnClient.GetInfo(ctxc, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("GetInfo failed: %w", err)
	}

	ownPubkeyBytes, err := hex.DecodeString(infoResp.IdentityPubkey)
	if err != nil {
		return fmt.Errorf("invalid own pubkey: %w", err)
	}

	lastHopBytes, err := hex.DecodeString(inChan.RemotePubkey)
	if err != nil {
		return fmt.Errorf("invalid in_chan remote pubkey: %w", err)
	}

	// Create a hold-free invoice for the amount so we have a payment hash.
	invoiceResp, err := lnClient.AddInvoice(ctxc, &lnrpc.Invoice{
		Value: amount,
		Memo:  fmt.Sprintf("rebalance out=%d in=%d", outChanID, inChanID),
	})
	if err != nil {
		return fmt.Errorf("AddInvoice failed: %w", err)
	}

	fmt.Printf("Rebalancing %s from channel %d → channel %d\n",
		commaSats(amount), outChanID, inChanID)

	req := &routerrpc.SendPaymentRequest{
		Dest:              ownPubkeyBytes,
		Amt:               amount,
		PaymentHash:       invoiceResp.RHash,
		OutgoingChanIds:   []uint64{outChanID},
		LastHopPubkey:     lastHopBytes,
		AllowSelfPayment:  true,
		TimeoutSeconds:    int32(timeoutSecs),
	}

	maxFee := ctx.Int64("max_fee_sat")
	if maxFee > 0 {
		req.FeeLimitSat = maxFee
	}

	routerClient := routerrpc.NewRouterClient(conn)
	stream, err := routerClient.SendPaymentV2(ctxc, req)
	if err != nil {
		return fmt.Errorf("SendPaymentV2 failed: %w", err)
	}

	payment, err := PrintLivePayment(ctxc, stream, lnClient, ctx.Bool("json"))
	if err != nil {
		return fmt.Errorf("payment stream error: %w", err)
	}

	if payment.Status != lnrpc.Payment_SUCCEEDED {
		return fmt.Errorf("rebalance failed: %s", payment.FailureReason)
	}

	feeSat := payment.FeeSat
	if !ctx.Bool("json") {
		fmt.Printf("Rebalance succeeded. Fee paid: %s\n", commaSats(feeSat))
	}
	return nil
}
