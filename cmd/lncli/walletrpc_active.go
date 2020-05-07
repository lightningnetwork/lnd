// +build walletrpc

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/urfave/cli"
)

// walletCommands will return the set of commands to enable for walletrpc
// builds.
func walletCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "wallet",
			Category:    "Wallet",
			Usage:       "Interact with the wallet.",
			Description: "",
			Subcommands: []cli.Command{
				pendingSweepsCommand,
				bumpFeeCommand,
				bumpCloseFeeCommand,
				listSweepsCommand,
			},
		},
	}
}

func getWalletClient(ctx *cli.Context) (walletrpc.WalletKitClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}
	return walletrpc.NewWalletKitClient(conn), cleanUp
}

var pendingSweepsCommand = cli.Command{
	Name:      "pendingsweeps",
	Usage:     "List all outputs that are pending to be swept within lnd.",
	ArgsUsage: "",
	Description: `
	List all on-chain outputs that lnd is currently attempting to sweep
	within its central batching engine. Outputs with similar fee rates are
	batched together in order to sweep them within a single transaction.
	`,
	Flags:  []cli.Flag{},
	Action: actionDecorator(pendingSweeps),
}

func pendingSweeps(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.PendingSweepsRequest{}
	resp, err := client.PendingSweeps(ctxb, req)
	if err != nil {
		return err
	}

	// Sort them in ascending fee rate order for display purposes.
	sort.Slice(resp.PendingSweeps, func(i, j int) bool {
		return resp.PendingSweeps[i].SatPerByte <
			resp.PendingSweeps[j].SatPerByte
	})

	var pendingSweepsResp = struct {
		PendingSweeps []*PendingSweep `json:"pending_sweeps"`
	}{
		PendingSweeps: make([]*PendingSweep, 0, len(resp.PendingSweeps)),
	}

	for _, protoPendingSweep := range resp.PendingSweeps {
		pendingSweep := NewPendingSweepFromProto(protoPendingSweep)
		pendingSweepsResp.PendingSweeps = append(
			pendingSweepsResp.PendingSweeps, pendingSweep,
		)
	}

	printJSON(pendingSweepsResp)

	return nil
}

var bumpFeeCommand = cli.Command{
	Name:      "bumpfee",
	Usage:     "Bumps the fee of an arbitrary input/transaction.",
	ArgsUsage: "outpoint",
	Description: `
	This command takes a different approach than bitcoind's bumpfee command.
	lnd has a central batching engine in which inputs with similar fee rates
	are batched together to save on transaction fees. Due to this, we cannot
	rely on bumping the fee on a specific transaction, since transactions
	can change at any point with the addition of new inputs. The list of
	inputs that currently exist within lnd's central batching engine can be
	retrieved through lncli pendingsweeps.

	When bumping the fee of an input that currently exists within lnd's
	central batching engine, a higher fee transaction will be created that
	replaces the lower fee transaction through the Replace-By-Fee (RBF)
	policy.

	This command also serves useful when wanting to perform a
	Child-Pays-For-Parent (CPFP), where the child transaction pays for its
	parent's fee. This can be done by specifying an outpoint within the low
	fee transaction that is under the control of the wallet.

	A fee preference must be provided, either through the conf_target or
	sat_per_byte parameters.

	Note that this command currently doesn't perform any validation checks
	on the fee preference being provided. For now, the responsibility of
	ensuring that the new fee preference is sufficient is delegated to the
	user.

	The force flag enables sweeping of inputs that are negatively yielding.
	Normally it does not make sense to lose money on sweeping, unless a
	parent transaction needs to get confirmed and there is only a small
	output available to attach the child transaction to.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks that the output should " +
				"be swept on-chain within",
		},
		cli.Uint64Flag{
			Name: "sat_per_byte",
			Usage: "a manual fee expressed in sat/byte that " +
				"should be used when sweeping the output",
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "sweep even if the yield is negative",
		},
	},
	Action: actionDecorator(bumpFee),
}

func bumpFee(ctx *cli.Context) error {
	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "bumpfee")
	}

	// Validate and parse the relevant arguments/flags.
	protoOutPoint, err := NewProtoOutPoint(ctx.Args().Get(0))
	if err != nil {
		return err
	}

	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	resp, err := client.BumpFee(context.Background(), &walletrpc.BumpFeeRequest{
		Outpoint:   protoOutPoint,
		TargetConf: uint32(ctx.Uint64("conf_target")),
		SatPerByte: uint32(ctx.Uint64("sat_per_byte")),
		Force:      ctx.Bool("force"),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var bumpCloseFeeCommand = cli.Command{
	Name:      "bumpclosefee",
	Usage:     "Bumps the fee of a channel closing transaction.",
	ArgsUsage: "channel_point",
	Description: `
	This command allows the fee of a channel closing transaction to be
	increased by using the child-pays-for-parent mechanism. It will instruct
	the sweeper to sweep the anchor outputs of transactions in the set
	of valid commitments for the specified channel at the requested fee
	rate or confirmation target.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks that the output should " +
				"be swept on-chain within",
		},
		cli.Uint64Flag{
			Name: "sat_per_byte",
			Usage: "a manual fee expressed in sat/byte that " +
				"should be used when sweeping the output",
		},
	},
	Action: actionDecorator(bumpCloseFee),
}

func bumpCloseFee(ctx *cli.Context) error {
	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "bumpclosefee")
	}

	// Validate the channel point.
	channelPoint := ctx.Args().Get(0)
	_, err := NewProtoOutPoint(channelPoint)
	if err != nil {
		return err
	}

	// Fetch all waiting close channels.
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Fetch waiting close channel commitments.
	commitments, err := getWaitingCloseCommitments(client, channelPoint)
	if err != nil {
		return err
	}

	// Retrieve pending sweeps.
	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	ctxb := context.Background()
	sweeps, err := walletClient.PendingSweeps(
		ctxb, &walletrpc.PendingSweepsRequest{},
	)
	if err != nil {
		return err
	}

	// Match pending sweeps with commitments of the channel for which a bump
	// is requested and bump their fees.
	commitSet := map[string]struct{}{
		commitments.LocalTxid:  {},
		commitments.RemoteTxid: {},
	}
	if commitments.RemotePendingTxid != "" {
		commitSet[commitments.RemotePendingTxid] = struct{}{}
	}

	for _, sweep := range sweeps.PendingSweeps {
		// Only bump anchor sweeps.
		if sweep.WitnessType != walletrpc.WitnessType_COMMITMENT_ANCHOR {
			continue
		}

		// Skip unrelated sweeps.
		sweepTxID, err := chainhash.NewHash(sweep.Outpoint.TxidBytes)
		if err != nil {
			return err
		}
		if _, match := commitSet[sweepTxID.String()]; !match {
			continue
		}

		// Bump fee of the anchor sweep.
		fmt.Printf("Bumping fee of %v:%v\n",
			sweepTxID, sweep.Outpoint.OutputIndex)

		_, err = walletClient.BumpFee(ctxb, &walletrpc.BumpFeeRequest{
			Outpoint:   sweep.Outpoint,
			TargetConf: uint32(ctx.Uint64("conf_target")),
			SatPerByte: uint32(ctx.Uint64("sat_per_byte")),
			Force:      true,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func getWaitingCloseCommitments(client lnrpc.LightningClient,
	channelPoint string) (*lnrpc.PendingChannelsResponse_Commitments,
	error) {

	ctxb := context.Background()

	req := &lnrpc.PendingChannelsRequest{}
	resp, err := client.PendingChannels(ctxb, req)
	if err != nil {
		return nil, err
	}

	// Lookup the channel commit tx hashes.
	for _, channel := range resp.WaitingCloseChannels {
		if channel.Channel.ChannelPoint == channelPoint {
			return channel.Commitments, nil
		}
	}

	return nil, errors.New("channel not found")
}

var listSweepsCommand = cli.Command{
	Name:     "listsweeps",
	Category: "On-chain",
	Usage:    "Lists all sweeps that have been published by our node.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "lookup full transaction",
		},
	},
	Description: `
	Get a list of the hex-encoded transaction ids of every sweep that our
	node has published. Note that these sweeps may not be confirmed on chain
	yet, as we store them on transaction broadcast, not confirmation.

	If the verbose flag is set, the full set of transactions will be 
	returned, otherwise only the sweep transaction ids will be returned. 
	`,
	Action: actionDecorator(listSweeps),
}

func listSweeps(ctx *cli.Context) error {
	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	resp, err := client.ListSweeps(
		context.Background(), &walletrpc.ListSweepsRequest{
			Verbose: ctx.IsSet("verbose"),
		},
	)
	if err != nil {
		return err
	}

	printJSON(resp)

	return nil
}
