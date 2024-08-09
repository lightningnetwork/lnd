//go:build chainrpc
// +build chainrpc

package commands

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/urfave/cli"
)

// chainCommands will return the set of commands to enable for chainrpc builds.
func chainCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "chain",
			Category: "On-chain",
			Usage:    "Interact with the bitcoin blockchain.",
			Subcommands: []cli.Command{
				getBlockCommand,
				getBestBlockCommand,
				getBlockHashCommand,
				getBlockHeaderCommand,
			},
		},
	}
}

func getChainClient(ctx *cli.Context) (chainrpc.ChainKitClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return chainrpc.NewChainKitClient(conn), cleanUp
}

var getBlockCommand = cli.Command{
	Name:        "getblock",
	Category:    "On-chain",
	Usage:       "Get block by block hash.",
	Description: "Returns a block given the corresponding block hash.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "hash",
			Usage: "the target block hash",
		},
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "print entire block as JSON",
		},
	},
	Action: actionDecorator(getBlock),
}

func getBlock(ctx *cli.Context) error {
	ctxc := getContext()

	var (
		args            = ctx.Args()
		blockHashString string
	)

	verbose := false
	if ctx.IsSet("verbose") {
		verbose = true
	}

	switch {
	case ctx.IsSet("hash"):
		blockHashString = ctx.String("hash")

	case args.Present():
		blockHashString = args.First()

	default:
		return fmt.Errorf("hash argument missing")
	}

	blockHash, err := chainhash.NewHashFromStr(blockHashString)
	if err != nil {
		return err
	}

	client, cleanUp := getChainClient(ctx)
	defer cleanUp()

	req := &chainrpc.GetBlockRequest{BlockHash: blockHash.CloneBytes()}
	resp, err := client.GetBlock(ctxc, req)
	if err != nil {
		return err
	}

	// Convert raw block bytes into wire.MsgBlock.
	msgBlock := &wire.MsgBlock{}
	blockReader := bytes.NewReader(resp.RawBlock)
	err = msgBlock.Deserialize(blockReader)
	if err != nil {
		return err
	}

	if verbose {
		printJSON(msgBlock)
	} else {
		printJSON(msgBlock.Header)
	}

	return nil
}

var getBlockHeaderCommand = cli.Command{
	Name:        "getblockheader",
	Usage:       "Get a block header.",
	Category:    "On-chain",
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

	blockHash, err := chainhash.NewHashFromStr(args.First())
	if err != nil {
		return err
	}

	req := &chainrpc.GetBlockHeaderRequest{BlockHash: blockHash[:]}

	client, cleanUp := getChainClient(ctx)
	defer cleanUp()

	resp, err := client.GetBlockHeader(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var getBestBlockCommand = cli.Command{
	Name:     "getbestblock",
	Category: "On-chain",
	Usage:    "Get best block.",
	Description: "Returns the latest block hash and height from the " +
		"valid most-work chain.",
	Action: actionDecorator(getBestBlock),
}

func getBestBlock(ctx *cli.Context) error {
	ctxc := getContext()

	client, cleanUp := getChainClient(ctx)
	defer cleanUp()

	resp, err := client.GetBestBlock(ctxc, &chainrpc.GetBestBlockRequest{})
	if err != nil {
		return err
	}

	// Cast gRPC block hash bytes as chain hash type.
	var blockHash chainhash.Hash
	copy(blockHash[:], resp.BlockHash)

	printJSON(struct {
		BlockHash   chainhash.Hash `json:"block_hash"`
		BlockHeight int32          `json:"block_height"`
	}{
		BlockHash:   blockHash,
		BlockHeight: resp.BlockHeight,
	})

	return nil
}

var getBlockHashCommand = cli.Command{
	Name:     "getblockhash",
	Category: "On-chain",
	Usage:    "Get block hash by block height.",
	Description: "Returns the block hash from the best chain at a given " +
		"height.",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "height",
			Usage: "target block height",
		},
	},
	Action: actionDecorator(getBlockHash),
}

func getBlockHash(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg()+ctx.NumFlags() != 1 {
		return cli.ShowCommandHelp(ctx, "getblockhash")
	}

	var (
		args        = ctx.Args()
		blockHeight int64
	)

	switch {
	case ctx.IsSet("height"):
		blockHeight = ctx.Int64("height")

	case args.Present():
		blockHeightString := args.First()

		// Convert block height positional argument from string to
		// int64.
		var err error
		blockHeight, err = strconv.ParseInt(blockHeightString, 10, 64)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("block height argument missing")
	}

	client, cleanUp := getChainClient(ctx)
	defer cleanUp()

	req := &chainrpc.GetBlockHashRequest{BlockHeight: blockHeight}
	resp, err := client.GetBlockHash(ctxc, req)
	if err != nil {
		return err
	}

	// Cast gRPC block hash bytes as chain hash type.
	var blockHash chainhash.Hash
	copy(blockHash[:], resp.BlockHash)

	printJSON(struct {
		BlockHash chainhash.Hash `json:"block_hash"`
	}{
		BlockHash: blockHash,
	})

	return nil
}
