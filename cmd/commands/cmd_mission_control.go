package commands

import (
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
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
        Update the config values being used by mission control to calculate the
        probability that payment routes will succeed. The estimator type must be
        provided to set estimator-related parameters.`,
	Flags: []cli.Flag{
		// General settings.
		cli.UintFlag{
			Name: "pmtnr",
			Usage: "the number of payments mission control " +
				"should store",
		},
		cli.DurationFlag{
			Name: "failrelax",
			Usage: "the amount of time to wait after a failure " +
				"before raising failure amount",
		},
		// Probability estimator.
		cli.StringFlag{
			Name: "estimator",
			Usage: "the probability estimator to use, choose " +
				"between 'apriori' or 'bimodal' (bimodal is " +
				"experimental)",
		},
		// Apriori config.
		cli.DurationFlag{
			Name: "apriorihalflife",
			Usage: "the amount of time taken to restore a node " +
				"or channel to 50% probability of success.",
		},
		cli.Float64Flag{
			Name: "apriorihopprob",
			Usage: "the probability of success assigned " +
				"to hops that we have no information about",
		},
		cli.Float64Flag{
			Name: "aprioriweight",
			Usage: "the degree to which mission control should " +
				"rely on historical results, expressed as " +
				"value in [0, 1]",
		},
		cli.Float64Flag{
			Name: "aprioricapacityfraction",
			Usage: "the fraction of channels' capacities that is " +
				"considered liquid in pathfinding, a value " +
				"between [0.75-1.0]. a value of 1.0 disables " +
				"this feature.",
		},
		// Bimodal config.
		cli.DurationFlag{
			Name: "bimodaldecaytime",
			Usage: "the time span after which we phase out " +
				"learnings from previous payment attempts",
		},
		cli.Uint64Flag{
			Name: "bimodalscale",
			Usage: "controls the assumed channel liquidity " +
				"imbalance in the network, measured in msat. " +
				"a low value (compared to typical channel " +
				"capacity) anticipates unbalanced channels.",
		},
		cli.Float64Flag{
			Name: "bimodalweight",
			Usage: "controls the degree to which the probability " +
				"estimator takes into account other channels " +
				"of a router",
		},
	},
	Action: actionDecorator(setCfg),
}

func setCfg(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	// Fetch current mission control config which we update to create our
	// response.
	mcCfg, err := client.GetMissionControlConfig(
		ctxc, &routerrpc.GetMissionControlConfigRequest{},
	)
	if err != nil {
		return err
	}

	// haveValue is a helper variable to determine if a flag has been set or
	// the help should be displayed.
	var haveValue bool

	// Handle general mission control settings.
	if ctx.IsSet("pmtnr") {
		haveValue = true
		mcCfg.Config.MaximumPaymentResults = uint32(ctx.Int("pmtnr"))
	}
	if ctx.IsSet("failrelax") {
		haveValue = true
		mcCfg.Config.MinimumFailureRelaxInterval = uint64(ctx.Duration(
			"failrelax",
		).Seconds())
	}

	// We switch between estimators and set corresponding configs. If
	// estimator is not set, we ignore the values.
	if ctx.IsSet("estimator") {
		switch ctx.String("estimator") {
		case routing.AprioriEstimatorName:
			haveValue = true

			// If we switch from another estimator, initialize with
			// default values.
			if mcCfg.Config.Model !=
				routerrpc.MissionControlConfig_APRIORI {

				dCfg := routing.DefaultAprioriConfig()
				aParams := &routerrpc.AprioriParameters{
					HalfLifeSeconds: uint64(
						dCfg.PenaltyHalfLife.Seconds(),
					),
					HopProbability: dCfg.
						AprioriHopProbability,
					Weight:           dCfg.AprioriWeight,
					CapacityFraction: dCfg.CapacityFraction,
				}

				// We make sure the correct config is set.
				mcCfg.Config.EstimatorConfig =
					&routerrpc.MissionControlConfig_Apriori{
						Apriori: aParams,
					}
			}

			// We update all values for the apriori estimator.
			mcCfg.Config.Model = routerrpc.
				MissionControlConfig_APRIORI

			aCfg := mcCfg.Config.GetApriori()
			if ctx.IsSet("apriorihalflife") {
				aCfg.HalfLifeSeconds = uint64(ctx.Duration(
					"apriorihalflife",
				).Seconds())
			}

			if ctx.IsSet("apriorihopprob") {
				aCfg.HopProbability = ctx.Float64(
					"apriorihopprob",
				)
			}

			if ctx.IsSet("aprioriweight") {
				aCfg.Weight = ctx.Float64("aprioriweight")
			}

			if ctx.IsSet("aprioricapacityfraction") {
				aCfg.CapacityFraction =
					ctx.Float64("aprioricapacityfraction")
			}

		case routing.BimodalEstimatorName:
			haveValue = true

			// If we switch from another estimator, initialize with
			// default values.
			if mcCfg.Config.Model !=
				routerrpc.MissionControlConfig_BIMODAL {

				dCfg := routing.DefaultBimodalConfig()
				bParams := &routerrpc.BimodalParameters{
					DecayTime: uint64(
						dCfg.BimodalDecayTime.Seconds(),
					),
					ScaleMsat: uint64(
						dCfg.BimodalScaleMsat,
					),
					NodeWeight: dCfg.BimodalNodeWeight,
				}

				// We make sure the correct config is set.
				mcCfg.Config.EstimatorConfig =
					&routerrpc.MissionControlConfig_Bimodal{
						Bimodal: bParams,
					}
			}

			// We update all values for the bimodal estimator.
			mcCfg.Config.Model = routerrpc.
				MissionControlConfig_BIMODAL

			bCfg := mcCfg.Config.GetBimodal()
			if ctx.IsSet("bimodaldecaytime") {
				bCfg.DecayTime = uint64(ctx.Duration(
					"bimodaldecaytime",
				).Seconds())
			}

			if ctx.IsSet("bimodalscale") {
				bCfg.ScaleMsat = ctx.Uint64("bimodalscale")
			}

			if ctx.IsSet("bimodalweight") {
				bCfg.NodeWeight = ctx.Float64(
					"bimodalweight",
				)
			}

		default:
			return fmt.Errorf("unknown estimator %v",
				ctx.String("estimator"))
		}
	}

	if !haveValue {
		return cli.ShowCommandHelp(ctx, "setmccfg")
	}

	_, err = client.SetMissionControlConfig(
		ctxc, &routerrpc.SetMissionControlConfigRequest{
			Config: mcCfg.Config,
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
	Usage:     "Deprecated. Estimate a success probability.",
	ArgsUsage: "from-node to-node amt",
	Action:    actionDecorator(queryProb),
	Hidden:    true,
}

func queryProb(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	if len(args) != 3 {
		return cli.ShowCommandHelp(ctx, "queryprob")
	}

	fromNode, err := route.NewVertexFromStr(args.Get(0))
	if err != nil {
		return fmt.Errorf("invalid from node key: %w", err)
	}

	toNode, err := route.NewVertexFromStr(args.Get(1))
	if err != nil {
		return fmt.Errorf("invalid to node key: %w", err)
	}

	amtSat, err := strconv.ParseUint(args.Get(2), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid amt: %w", err)
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
