//go:build dev
// +build dev

package commands

import (
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/urfave/cli"
)

// devCommands will return the set of commands to enable for devrpc builds.
func devCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "importgraph",
			Category:    "Development",
			Description: "Imports graph from describegraph JSON",
			Usage:       "Import the network graph.",
			ArgsUsage:   "graph-json-file",
			Action:      actionDecorator(importGraph),
		},
	}
}

func getDevClient(ctx *cli.Context) (devrpc.DevClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}
	return devrpc.NewDevClient(conn), cleanUp
}

func importGraph(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getDevClient(ctx)
	defer cleanUp()

	jsonFile := lncfg.CleanAndExpandPath(ctx.Args().First())
	jsonBytes, err := os.ReadFile(jsonFile)
	if err != nil {
		return fmt.Errorf("error reading JSON from file %v: %v",
			jsonFile, err)
	}

	jsonGraph := &lnrpc.ChannelGraph{}
	err = lnrpc.ProtoJSONUnmarshalOpts.Unmarshal(jsonBytes, jsonGraph)
	if err != nil {
		return fmt.Errorf("error parsing JSON: %w", err)
	}
	res, err := client.ImportGraph(ctxc, jsonGraph)
	if err != nil {
		return err
	}

	printRespJSON(res)
	return nil
}
