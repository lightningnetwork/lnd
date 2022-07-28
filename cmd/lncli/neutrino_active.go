//go:build neutrinorpc
// +build neutrinorpc

package main

import (
	"fmt"
	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/urfave/cli"
)

func getNeutrinoKitClient(ctx *cli.Context) (neutrinorpc.NeutrinoKitClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return neutrinorpc.NewNeutrinoKitClient(conn), cleanUp
}

var getNeutrinoStatusCommand = cli.Command{
	Name:     "status",
	Usage:    "Returns the status of the running neutrino instance.",
	Category: "Neutrino",
	Description: "Returns the status of the light client neutrino " +
		"instance, along with height and hash of the best block, and " +
		"a list of connected peers.",
	Action: actionDecorator(getNeutrinoStatus),
}

func getNeutrinoStatus(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.StatusRequest{}

	resp, err := client.Status(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var addPeerCommand = cli.Command{
	Name:     "addpeer",
	Usage:    "Add a peer.",
	Category: "Neutrino",
	Description: "Adds a new peer that has already been connected to the " +
		"server.",
	ArgsUsage: "address",
	Action:    actionDecorator(addNeutrinoPeer),
}

func addNeutrinoPeer(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 || ctx.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, "addpeer")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.AddPeerRequest{
		PeerAddrs: ctx.Args().First(),
	}

	// Add a peer to the neutrino server.
	resp, err := client.AddPeer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var disconnectPeerCommand = cli.Command{
	Name:     "disconnectpeer",
	Usage:    "Disconnect a peer.",
	Category: "Neutrino",
	Description: "Disconnects a peer by target address. Both outbound and" +
		"inbound nodes will be searched for the target node. An error " +
		"message will be returned if the peer was not found.",
	ArgsUsage: "address",
	Action:    actionDecorator(disconnectNeutrinoPeer),
}

func disconnectNeutrinoPeer(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 || ctx.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, "disconnectpeer")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.DisconnectPeerRequest{
		PeerAddrs: ctx.Args().First(),
	}

	// Disconnect a peer to the neutrino server.
	resp, err := client.DisconnectPeer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var isBannedCommand = cli.Command{
	Name:        "isbanned",
	Usage:       "Get ban status.",
	Category:    "Neutrino",
	Description: "Returns true if the peer is banned, otherwise false.",
	ArgsUsage:   "address",
	Action:      actionDecorator(isBanned),
}

func isBanned(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "isbanned")
	}
	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.IsBannedRequest{
		PeerAddrs: ctx.Args().First(),
	}

	// Check if the peer is banned.
	resp, err := client.IsBanned(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getBlockHeaderCommand = cli.Command{
	Name:        "getblockheader",
	Usage:       "Get a block header.",
	Category:    "Neutrino",
	Description: "Returns a block header with a particular block hash.",
	ArgsUsage:   "hash",
	Action:      actionDecorator(getBlockHeader),
}

func getBlockHeader(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if !args.Present() {
		return cli.ShowCommandHelp(ctx, "getblockheader")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.GetBlockHeaderRequest{
		Hash: ctx.Args().First(),
	}

	resp, err := client.GetBlockHeader(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getBlockCommand = cli.Command{
	Name:        "getblock",
	Usage:       "Get a block.",
	Category:    "Neutrino",
	Description: "Returns a block with a particular block hash.",
	ArgsUsage:   "hash",
	Action:      actionDecorator(getBlock),
}

func getBlock(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if !args.Present() {
		return cli.ShowCommandHelp(ctx, "getblock")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.GetBlockRequest{
		Hash: args.First(),
	}

	resp, err := client.GetBlock(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getCFilterCommand = cli.Command{
	Name:        "getcfilter",
	Usage:       "Get a compact filter.",
	Category:    "Neutrino",
	Description: "Returns a compact filter of a particular block.",
	ArgsUsage:   "hash",
	Action:      actionDecorator(getCFilter),
}

func getCFilter(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if !args.Present() {
		return cli.ShowCommandHelp(ctx, "getcfilter")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.GetCFilterRequest{Hash: args.First()}

	resp, err := client.GetCFilter(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var findTxsCommand = cli.Command{
	Name:     "txscan",
	Usage:    "Scan for transactions.",
	Category: "Neutrino",
	Description: "Trigger a rescan for transactions containing any of the " +
		"given addresses. This may take some time depending on the initial " +
		"conditions.",
	ArgsUsage: "[flags] <addr> <addr> <addr>",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "start_block",
			Usage: "Start the rescan at this block height. If no start block is " +
				"specified, the scan will be started from current best block.",
		},
		cli.Int64Flag{
			Name:  "end_block",
			Usage: "End the rescan at this block height.",
		},
		cli.Int64Flag{
			Name:  "timeout",
			Usage: "Rescan timeout (in seconds).",
		},
	},
	Action: actionDecorator(findTxsFunc),
}

func findTxsFunc(ctx *cli.Context) error {
	ctxc := getContext()
	args := ctx.Args()

	if !args.Present() {
		return fmt.Errorf("txs argument missing")
	}

	if !ctx.IsSet("end_block") {
		return fmt.Errorf("end_block must be set")
	}

	if !ctx.IsSet("timeout") {
		return fmt.Errorf("timeout must be set")
	}

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	var txs = make([]string, ctx.NArg())
	for i := range args {
		txs[i] = args.Get(i)
	}

	startBlock := ctx.Int64("start_block")
	endBlock := ctx.Int64("end_block")
	timeout := ctx.Int64("timeout")

	if endBlock == 0 {
		return fmt.Errorf(
			"end_block must be greater than zero",
		)
	} else if endBlock < startBlock {
		return fmt.Errorf(
			"end_block must be greater than start_block",
		)
	}

	if timeout < 0 {
		return fmt.Errorf("timeout can not be negative")
	}

	req := &neutrinorpc.TxScanRequest{
		Addrs:      txs,
		StartBlock: startBlock,
		EndBlock:   endBlock,
		Timeout:    timeout,
	}

	resp, err := client.TxScan(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var stopTxScanCommad = cli.Command{
	Name:        "stoptxscan",
	Usage:       "Stop any tx scan.",
	Category:    "Neutrino",
	Description: "",
	ArgsUsage:   "",
	Action:      actionDecorator(stopTxScanFunc),
}

func stopTxScanFunc(ctx *cli.Context) error {
	ctxc := getContext()

	client, cleanUp := getNeutrinoKitClient(ctx)
	defer cleanUp()

	req := &neutrinorpc.StopTxScanRequest{}

	resp, err := client.StopTxScan(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

// neutrinoCommands will return the set of commands to enable for neutrinorpc
// builds.
func neutrinoCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "neutrino",
			Category:    "Neutrino",
			Usage:       "Interact with a running neutrino instance.",
			Description: "",
			Subcommands: []cli.Command{
				getNeutrinoStatusCommand,
				addPeerCommand,
				disconnectPeerCommand,
				isBannedCommand,
				getBlockCommand,
				getBlockHeaderCommand,
				getCFilterCommand,
				findTxsCommand,
				stopTxScanCommad,
			},
		},
	}
}
