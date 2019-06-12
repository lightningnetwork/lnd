// +build walletrpc

package main

import (
	"context"
	"fmt"
	"sort"

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
	user.`,
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
	Action: actionDecorator(bumpFee),
}

func bumpFee(ctx *cli.Context) error {
	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 || ctx.NumFlags() != 1 {
		return cli.ShowCommandHelp(ctx, "bumpfee")
	}

	// Validate and parse the relevant arguments/flags.
	protoOutPoint, err := NewProtoOutPoint(ctx.Args().Get(0))
	if err != nil {
		return err
	}

	var confTarget, satPerByte uint32
	switch {
	case ctx.IsSet("conf_target") && ctx.IsSet("sat_per_byte"):
		return fmt.Errorf("either conf_target or sat_per_byte should " +
			"be set, but not both")
	case ctx.IsSet("conf_target"):
		confTarget = uint32(ctx.Uint64("conf_target"))
	case ctx.IsSet("sat_per_byte"):
		satPerByte = uint32(ctx.Uint64("sat_per_byte"))
	}

	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	resp, err := client.BumpFee(context.Background(), &walletrpc.BumpFeeRequest{
		Outpoint:   protoOutPoint,
		TargetConf: confTarget,
		SatPerByte: satPerByte,
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
