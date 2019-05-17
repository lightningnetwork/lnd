// +build walletrpc

package main

import (
	"context"
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
