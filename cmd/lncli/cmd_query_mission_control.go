// +build routerrpc

package main

import (
	"context"
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"

	"github.com/urfave/cli"
)

var queryMissionControlCommand = cli.Command{
	Name:     "querymc",
	Category: "Payments",
	Usage:    "Query the internal mission control state.",
	Action:   actionDecorator(queryMissionControl),
}

func queryMissionControl(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.QueryMissionControlRequest{}
	rpcCtx := context.Background()
	snapshot, err := client.QueryMissionControl(rpcCtx, req)
	if err != nil {
		return err
	}

	type displayNodeHistory struct {
		Pubkey               string
		LastFailTime         int64
		OtherChanSuccessProb float32
	}

	type displayPairHistory struct {
		NodeA, NodeB string
		Timestamp    int64
		SuccessProb  float32
		Amt          int64
		Result       string
	}

	displayResp := struct {
		Nodes []displayNodeHistory
		Pairs []displayPairHistory
	}{}

	for _, n := range snapshot.Nodes {
		displayResp.Nodes = append(
			displayResp.Nodes,
			displayNodeHistory{
				Pubkey:               hex.EncodeToString(n.Pubkey),
				LastFailTime:         n.LastFailTime,
				OtherChanSuccessProb: n.OtherChanSuccessProb,
			},
		)
	}

	for _, n := range snapshot.Pairs {
		displayResp.Pairs = append(
			displayResp.Pairs,
			displayPairHistory{
				NodeA:       hex.EncodeToString(n.NodeA),
				NodeB:       hex.EncodeToString(n.NodeB),
				Timestamp:   n.Timestamp,
				SuccessProb: n.SuccessProb,
				Amt:         n.Amt,
				Result:      n.Result.String(),
			},
		)
	}

	printJSON(displayResp)

	return nil
}
