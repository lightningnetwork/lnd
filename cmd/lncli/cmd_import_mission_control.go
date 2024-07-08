package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

const argsStr = "[source node] [dest node] [unix ts seconds] [amount in msat]"

var importMissionControlCommand = cli.Command{
	Name:      "importmc",
	Category:  "Payments",
	Usage:     "Import a result to the internal mission control state.",
	ArgsUsage: fmt.Sprintf("importmc %v", argsStr),
	Action:    actionDecorator(importMissionControl),
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "failure",
			Usage: "whether the routing history entry was a failure",
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "whether to force the history entry import",
		},
		cli.BoolFlag{
			Name: "persist_mc",
			Usage: "whether to persist the imported mission " +
				"control to disk",
		},
	},
}

func importMissionControl(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	if ctx.NArg() != 4 {
		return fmt.Errorf("please provide args: %v", argsStr)
	}

	args := ctx.Args()

	sourceNode, err := route.NewVertexFromStr(args[0])
	if err != nil {
		return fmt.Errorf("please provide valid source node: %w", err)
	}

	destNode, err := route.NewVertexFromStr(args[1])
	if err != nil {
		return fmt.Errorf("please provide valid dest node: %w", err)
	}

	ts, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return fmt.Errorf("please provide unix timestamp "+
			"in seconds: %v", err)
	}

	if ts <= 0 {
		return errors.New("please provide positive timestamp")
	}

	amt, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return fmt.Errorf("please provide amount in msat: %w", err)
	}

	// Allow 0 value as failure amount.
	if !ctx.IsSet("failure") && amt <= 0 {
		return errors.New("success amount must be >0")
	}

	client := routerrpc.NewRouterClient(conn)

	importResult := &routerrpc.PairHistory{
		NodeFrom: sourceNode[:],
		NodeTo:   destNode[:],
		History:  &routerrpc.PairData{},
	}

	if ctx.IsSet("failure") {
		importResult.History.FailAmtMsat = amt
		importResult.History.FailTime = ts
	} else {
		importResult.History.SuccessAmtMsat = amt
		importResult.History.SuccessTime = ts
	}

	req := &routerrpc.XImportMissionControlRequest{
		Pairs: []*routerrpc.PairHistory{
			importResult,
		},
		Force:     ctx.IsSet("force"),
		PersistMc: ctx.IsSet("persist_mc"),
	}

	rpcCtx := context.Background()
	_, err = client.XImportMissionControl(rpcCtx, req)
	return err
}
