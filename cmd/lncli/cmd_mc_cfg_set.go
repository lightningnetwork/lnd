package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var setCfgCommand = cli.Command{
	Name:  "setmccfg",
	Usage: "Set mission control's config.",
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
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	ctxb := context.Background()
	resp, err := client.GetMissionControlConfig(
		ctxb, &routerrpc.GetMissionControlConfigRequest{},
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
		ctxb, &routerrpc.SetMissionControlConfigRequest{
			Config: resp.Config,
		},
	)
	return err
}
