//go:build neutrinorpc
// +build neutrinorpc

package commands

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/urfave/cli/v3"
)

func getNeutrinoKitClient(cmd *cli.Command) (neutrinorpc.NeutrinoKitClient, func()) {
	conn := getClientConn(cmd, false)

	cleanUp := func() {
		conn.Close()
	}

	return neutrinorpc.NewNeutrinoKitClient(conn), cleanUp
}

var getNeutrinoStatusCommand = &cli.Command{
	Name:     "status",
	Usage:    "Returns the status of the running neutrino instance.",
	Category: "Neutrino",
	Description: "Returns the status of the light client neutrino " +
		"instance, along with height and hash of the best block, and " +
		"a list of connected peers.",
	Action: actionDecorator(getNeutrinoStatus),
}

func getNeutrinoStatus(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.StatusRequest{}

	resp, err := client.Status(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var addPeerCommand = &cli.Command{
	Name:     "addpeer",
	Usage:    "Add a peer.",
	Category: "Neutrino",
	Description: "Adds a new peer that has already been connected to the " +
		"server.",
	ArgsUsage: "address",
	Action:    actionDecorator(addNeutrinoPeer),
}

func addNeutrinoPeer(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if cmd.NArg() != 1 || cmd.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, cmd, "addpeer")
	}

	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.AddPeerRequest{
		PeerAddrs: cmd.Args().First(),
	}

	// Add a peer to the neutrino server.
	resp, err := client.AddPeer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var disconnectPeerCommand = &cli.Command{
	Name:     "disconnectpeer",
	Usage:    "Disconnect a peer.",
	Category: "Neutrino",
	Description: "Disconnects a peer by target address. Both outbound and" +
		"inbound nodes will be searched for the target node. An error " +
		"message will be returned if the peer was not found.",
	ArgsUsage: "address",
	Action:    actionDecorator(disconnectNeutrinoPeer),
}

func disconnectNeutrinoPeer(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if cmd.NArg() != 1 || cmd.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, cmd, "disconnectpeer")
	}

	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.DisconnectPeerRequest{
		PeerAddrs: cmd.Args().First(),
	}

	// Disconnect a peer to the neutrino server.
	resp, err := client.DisconnectPeer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var isBannedCommand = &cli.Command{
	Name:        "isbanned",
	Usage:       "Get ban status.",
	Category:    "Neutrino",
	Description: "Returns true if the peer is banned, otherwise false.",
	ArgsUsage:   "address",
	Action:      actionDecorator(isBanned),
}

func isBanned(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if cmd.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, cmd, "isbanned")
	}
	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.IsBannedRequest{
		PeerAddrs: cmd.Args().First(),
	}

	// Check if the peer is banned.
	resp, err := client.IsBanned(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getBlockHeaderNeutrinoCommand = &cli.Command{
	Name:        "getblockheader",
	Usage:       "Get a block header.",
	Category:    "Neutrino",
	Description: "Returns a block header with a particular block hash.",
	ArgsUsage:   "hash",
	Action:      actionDecorator(getBlockHeaderNeutrino),
}

func getBlockHeaderNeutrino(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	args := cmd.Args().Slice()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if len(args) == 0 {
		return cli.ShowCommandHelp(ctx, cmd, "getblockheader")
	}

	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.GetBlockHeaderRequest{
		Hash: cmd.Args().First(),
	}

	resp, err := client.GetBlockHeader(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getCFilterCommand = &cli.Command{
	Name:        "getcfilter",
	Usage:       "Get a compact filter.",
	Category:    "Neutrino",
	Description: "Returns a compact filter of a particular block.",
	ArgsUsage:   "hash",
	Action:      actionDecorator(getCFilter),
}

func getCFilter(ctx context.Context, cmd *cli.Command) error {
	ctxc := getContext()
	args := cmd.Args().Slice()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if len(args) == 0 {
		return cli.ShowCommandHelp(ctx, cmd, "getcfilter")
	}

	client, cleanUp := getNeutrinoKitClient(cmd)
	defer cleanUp()

	req := &neutrinorpc.GetCFilterRequest{Hash: args[0]}

	resp, err := client.GetCFilter(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

// neutrinoCommands will return the set of commands to enable for neutrinorpc
// builds.
func neutrinoCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:        "neutrino",
			Category:    "Neutrino",
			Usage:       "Interact with a running neutrino instance.",
			Description: "",
			Commands: []*cli.Command{
				getNeutrinoStatusCommand,
				addPeerCommand,
				disconnectPeerCommand,
				isBannedCommand,
				getBlockHeaderNeutrinoCommand,
				getCFilterCommand,
			},
		},
	}
}
