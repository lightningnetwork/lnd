package main

import (
	"fmt"
	"strconv"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

var getCfgCommand = cli.Command{
	Name:     "getmccfg",
	Category: "Mission Control",
	Usage:    "Display mission control's config.",
	Description: `
	Returns the config currently being used by mission control.
	`,
	Action: actionDecorator(getCfg),
}

func getCfg(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	resp, err := client.GetMissionControlConfig(
		ctxc, &routerrpc.GetMissionControlConfigRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var setCfgCommand = cli.Command{
	Name:     "setmccfg",
	Category: "Mission Control",
	Usage:    "Set mission control's config.",
	Description: `
	Update the config values being used by mission control to calculate 
	the probability that payment routes will succeed.
	`,
	Flags: []cli.Flag{
		cli.DurationFlag{
			Name: "halflife",
			Usage: "the amount of time taken to restore a node " +
				"or channel to 50% probability of success.",
		},
		cli.Float64Flag{
			Name: "hopprob",
			Usage: "the probability of success assigned " +
				"to hops that we have no information about",
		},
		cli.Float64Flag{
			Name: "weight",
			Usage: "the degree to which mission control should " +
				"rely on historical results, expressed as " +
				"value in [0;1]",
		}, cli.UintFlag{
			Name: "pmtnr",
			Usage: "the number of payments mission control " +
				"should store",
		},
		cli.DurationFlag{
			Name: "failrelax",
			Usage: "the amount of time to wait after a failure " +
				"before raising failure amount",
		},
	},
	Action: actionDecorator(setCfg),
}

func setCfg(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	resp, err := client.GetMissionControlConfig(
		ctxc, &routerrpc.GetMissionControlConfigRequest{},
	)
	if err != nil {
		return err
	}

	var haveValue bool

	if ctx.IsSet("halflife") {
		haveValue = true
		resp.Config.HalfLifeSeconds = uint64(ctx.Duration(
			"halflife",
		).Seconds())
	}

	if ctx.IsSet("hopprob") {
		haveValue = true
		resp.Config.HopProbability = float32(ctx.Float64("hopprob"))
	}

	if ctx.IsSet("weight") {
		haveValue = true
		resp.Config.Weight = float32(ctx.Float64("weight"))
	}

	if ctx.IsSet("pmtnr") {
		haveValue = true
		resp.Config.MaximumPaymentResults = uint32(ctx.Int("pmtnr"))
	}

	if ctx.IsSet("failrelax") {
		haveValue = true
		resp.Config.MinimumFailureRelaxInterval = uint64(ctx.Duration(
			"failrelax",
		).Seconds())
	}

	if !haveValue {
		return cli.ShowCommandHelp(ctx, "setmccfg")
	}

	_, err = client.SetMissionControlConfig(
		ctxc, &routerrpc.SetMissionControlConfigRequest{
			Config: resp.Config,
		},
	)
	return err
}

var queryMissionControlCommand = cli.Command{
	Name:     "querymc",
	Category: "Mission Control",
	Usage:    "Query the internal mission control state.",
	Action:   actionDecorator(queryMissionControl),
}

func queryMissionControl(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.QueryMissionControlRequest{}
	snapshot, err := client.QueryMissionControl(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(snapshot)

	return nil
}

var queryProbCommand = cli.Command{
	Name:      "queryprob",
	Category:  "Mission Control",
	Usage:     "Estimate a success probability.",
	ArgsUsage: "from-node to-node amt",
	Action:    actionDecorator(queryProb),
}

func queryProb(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	if len(args) != 3 {
		return cli.ShowCommandHelp(ctx, "queryprob")
	}

	fromNode, err := route.NewVertexFromStr(args.Get(0))
	if err != nil {
		return fmt.Errorf("invalid from node key: %v", err)
	}

	toNode, err := route.NewVertexFromStr(args.Get(1))
	if err != nil {
		return fmt.Errorf("invalid to node key: %v", err)
	}

	amtSat, err := strconv.ParseUint(args.Get(2), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid amt: %v", err)
	}

	amtMsat := lnwire.NewMSatFromSatoshis(
		btcutil.Amount(amtSat),
	)

	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.QueryProbabilityRequest{
		FromNode: fromNode[:],
		ToNode:   toNode[:],
		AmtMsat:  int64(amtMsat),
	}

	response, err := client.QueryProbability(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(response)

	return nil
}

var resetMissionControlCommand = cli.Command{
	Name:     "resetmc",
	Category: "Mission Control",
	Usage:    "Reset internal mission control state.",
	Action:   actionDecorator(resetMissionControl),
}

func resetMissionControl(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.ResetMissionControlRequest{}
	_, err := client.ResetMissionControl(ctxc, req)
	return err
}
