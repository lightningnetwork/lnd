// +build routerrpc

package main

import (
	"context"
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"

	"github.com/urfave/cli"
)

var queryReputationsCommand = cli.Command{
	Name:     "queryreputations",
	Category: "Payments",
	Usage:    "Query the reputation data that has been collected.",
	Action:   actionDecorator(queryReputations),
}

func queryReputations(ctx *cli.Context) error {
	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.QueryReputationsRequest{}
	rpcCtx := context.Background()
	snapshot, err := client.QueryReputations(rpcCtx, req)
	if err != nil {
		return err
	}

	type displayNodeHistory struct {
		Pubkey               string
		LastFailTime         int64
		OtherChanSuccessProb float32
		Channels             []*routerrpc.ChannelHistory
	}

	displayResp := struct {
		Nodes []displayNodeHistory
	}{}

	for _, n := range snapshot.Nodes {
		displayResp.Nodes = append(
			displayResp.Nodes,
			displayNodeHistory{
				Pubkey:               hex.EncodeToString(n.Pubkey),
				LastFailTime:         n.LastFailTime,
				OtherChanSuccessProb: n.OtherChanSuccessProb,
				Channels:             n.Channels,
			},
		)
	}

	printJSON(displayResp)

	return nil
}
