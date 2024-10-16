package commands

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var updateChanStatusCommand = cli.Command{
	Name:     "updatechanstatus",
	Category: "Channels",
	Usage:    "Set the status of an existing channel on the network.",
	Description: `
	Set the status of an existing channel on the network. The actions can
	be "enable", "disable", or "auto". If the action changes the status, a
	message will be broadcast over the network.

	Note that enabling / disabling a channel using this command ONLY affects
	what's advertised over the network. For example, disabling a channel
	using this command does not close it.

	If a channel is manually disabled, automatic / background requests to
	re-enable the channel will be ignored. However, if a channel is
	manually enabled, automatic / background requests to disable the
	channel will succeed (such requests are usually made on channel close
	or when the peer is down).

	The "auto" action restores automatic channel state management. Per
	the behavior described above, it's only needed to undo the effect of
	a prior "disable" action, and will be a no-op otherwise.`,
	ArgsUsage: "funding_txid [output_index] action",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "funding_txid",
			Usage: "the txid of the channel's funding transaction",
		},
		cli.IntFlag{
			Name: "output_index",
			Usage: "the output index for the funding output of " +
				"the funding transaction",
		},
		cli.StringFlag{
			Name: "chan_point",
			Usage: "the channel whose status should be updated. " +
				"Takes the form of: txid:output_index",
		},
		cli.StringFlag{
			Name: "action",
			Usage: `the action to take: must be one of "enable", ` +
				`"disable", or "auto"`,
		},
	},
	Action: actionDecorator(updateChanStatus),
}

func updateChanStatus(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "updatechanstatus")
		return nil
	}

	channelPoint, err := parseChannelPoint(ctx)
	if err != nil {
		return err
	}

	var action routerrpc.ChanStatusAction
	switch ctx.String("action") {
	case "enable":
		action = routerrpc.ChanStatusAction_ENABLE
	case "disable":
		action = routerrpc.ChanStatusAction_DISABLE
	case "auto":
		action = routerrpc.ChanStatusAction_AUTO
	default:
		return errors.New(`action must be one of "enable", "disable", ` +
			`or "auto"`)
	}
	req := &routerrpc.UpdateChanStatusRequest{
		ChanPoint: channelPoint,
		Action:    action,
	}

	client := routerrpc.NewRouterClient(conn)
	resp, err := client.UpdateChanStatus(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
