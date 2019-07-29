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
		Pubkey           string
		LastFailTime     int64
		OtherSuccessProb float32
	}

	type displayPairHistory struct {
		NodeFrom, NodeTo  string
		LastFailTime      int64
		SuccessProb       float32
		MinPenalizeAmtSat int64
	}

	displayResp := struct {
		Nodes []displayNodeHistory
		Pairs []displayPairHistory
	}{}

	for _, n := range snapshot.Nodes {
		displayResp.Nodes = append(
			displayResp.Nodes,
			displayNodeHistory{
				Pubkey:           hex.EncodeToString(n.Pubkey),
				LastFailTime:     n.LastFailTime,
				OtherSuccessProb: n.OtherSuccessProb,
			},
		)
	}

	for _, n := range snapshot.Pairs {
		displayResp.Pairs = append(
			displayResp.Pairs,
			displayPairHistory{
				NodeFrom:          hex.EncodeToString(n.NodeFrom),
				NodeTo:            hex.EncodeToString(n.NodeTo),
				LastFailTime:      n.LastFailTime,
				SuccessProb:       n.SuccessProb,
				MinPenalizeAmtSat: n.MinPenalizeAmtSat,
			},
		)
	}

	printJSON(displayResp)

	return nil
}
