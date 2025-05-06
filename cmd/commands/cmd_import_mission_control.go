package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

const argsStr = "[source node] [dest node] [unix ts seconds] [amount in msat]"

var importMissionControlCommand = cli.Command{
	Name:      "importmc",
	Category:  "Mission Control",
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
		Force: ctx.IsSet("force"),
	}

	rpcCtx := context.Background()
	_, err = client.XImportMissionControl(rpcCtx, req)
	return err
}

var loadMissionControlCommand = cli.Command{
	Name:     "loadmc",
	Category: "Mission Control",
	Usage: "Load mission control results to the internal mission " +
		"control state from a file produced by `querymc` with the " +
		"option to shift timestamps. Note that this data is not " +
		"persisted across restarts.",
	Action: actionDecorator(loadMissionControl),
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "mcdatapath",
			Usage: "The path to the querymc output file (json).",
		},
		cli.StringFlag{
			Name: "timeoffset",
			Usage: "Time offset to add to all timestamps. " +
				"Follows a format like 72h3m0.5s. " +
				"This can be used to make mission control " +
				"data appear more recent, to trick " +
				"pathfinding's in-built information decay " +
				"mechanism. Additionally, " +
				"by setting 0s, this will report the most " +
				"recent result timestamp, which can be used " +
				"to find out how old this data is.",
		},
		cli.BoolFlag{
			Name: "force",
			Usage: "Whether to force overiding more recent " +
				"results in the database with older results " +
				"from the file.",
		},
		cli.BoolFlag{
			Name: "skip_confirmation",
			Usage: "Skip the confirmation prompt and import " +
				"immediately",
		},
	},
}

// loadMissionControl loads mission control data into an LND instance.
func loadMissionControl(ctx *cli.Context) error {
	rpcCtx := context.Background()

	mcDataPath := ctx.String("mcdatapath")
	if mcDataPath == "" {
		return fmt.Errorf("mcdatapath must be set")
	}

	if _, err := os.Stat(mcDataPath); os.IsNotExist(err) {
		return fmt.Errorf("%v does not exist", mcDataPath)
	}

	// Load and unmarshal the querymc output file.
	mcRaw, err := os.ReadFile(mcDataPath)
	if err != nil {
		return fmt.Errorf("could not read querymc output file: %w", err)
	}

	mc := &routerrpc.QueryMissionControlResponse{}
	err = lnrpc.ProtoJSONUnmarshalOpts.Unmarshal(mcRaw, mc)
	if err != nil {
		return fmt.Errorf("could not unmarshal querymc output file: %w",
			err)
	}

	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	// Add a time offset to all timestamps if requested.
	timeOffset := ctx.String("timeoffset")
	if timeOffset != "" {
		offset, err := time.ParseDuration(timeOffset)
		if err != nil {
			return fmt.Errorf("could not parse time offset: %w",
				err)
		}

		var maxTimestamp time.Time

		for _, pair := range mc.Pairs {
			if pair.History.SuccessTime != 0 {
				unix := time.Unix(pair.History.SuccessTime, 0)
				unix = unix.Add(offset)

				if unix.After(maxTimestamp) {
					maxTimestamp = unix
				}

				pair.History.SuccessTime = unix.Unix()
			}

			if pair.History.FailTime != 0 {
				unix := time.Unix(pair.History.FailTime, 0)
				unix = unix.Add(offset)

				if unix.After(maxTimestamp) {
					maxTimestamp = unix
				}

				pair.History.FailTime = unix.Unix()
			}
		}

		fmt.Printf("Added a time offset %v to all timestamps. "+
			"New max timestamp: %v\n", offset, maxTimestamp)
	}

	sanitizeMCData(mc.Pairs)

	fmt.Printf("Mission control file contains %v pairs.\n", len(mc.Pairs))
	if !ctx.Bool("skip_confirmation") &&
		!promptForConfirmation(
			"Import mission control data (yes/no): ",
		) {

		return nil
	}

	_, err = client.XImportMissionControl(
		rpcCtx,
		&routerrpc.XImportMissionControlRequest{
			Pairs: mc.Pairs, Force: ctx.Bool("force"),
		},
	)
	if err != nil {
		return fmt.Errorf("could not import mission control data: %w",
			err)
	}

	return nil
}

// sanitizeMCData removes invalid data from the exported mission control data.
func sanitizeMCData(mc []*routerrpc.PairHistory) {
	for _, pair := range mc {
		// It is not allowed to import a zero-amount success to mission
		// control if a timestamp is set. We unset it in this case.
		if pair.History.SuccessTime != 0 &&
			pair.History.SuccessAmtMsat == 0 &&
			pair.History.SuccessAmtSat == 0 {

			pair.History.SuccessTime = 0
		}

		// If we only deal with a failure, we need to set the failure
		// amount to a tiny value due to a limitation in the RPC. This
		// will lead to a similar penalization in pathfinding.
		if pair.History.SuccessTime == 0 &&
			pair.History.FailTime != 0 &&
			pair.History.FailAmtMsat == 0 &&
			pair.History.FailAmtSat == 0 {

			pair.History.FailAmtMsat = 1
		}
	}
}
