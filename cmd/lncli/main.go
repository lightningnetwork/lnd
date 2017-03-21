package main

import (
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"

	"google.golang.org/grpc"
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[lncli] %v\n", err)
	os.Exit(1)
}

func getClient(ctx *cli.Context) (lnrpc.LightningClient, func()) {
	conn := getClientConn(ctx)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewLightningClient(conn), cleanUp
}

func getClientConn(ctx *cli.Context) *grpc.ClientConn {
	// TODO(roasbeef): macaroon based auth
	// * http://www.grpc.io/docs/guides/auth.html
	// * http://research.google.com/pubs/pub41892.html
	// * https://github.com/go-macaroon/macaroon
	opts := []grpc.DialOption{grpc.WithInsecure()}

	conn, err := grpc.Dial(ctx.GlobalString("rpcserver"), opts...)
	if err != nil {
		fatal(err)
	}

	return conn
}

func main() {
	app := cli.NewApp()
	app.Name = "lncli"
	app.Version = "0.1"
	app.Usage = "control plane for your Lightning Network Daemon (lnd)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:10009",
			Usage: "host:port of ln daemon",
		},
	}
	app.Commands = []cli.Command{
		newAddressCommand,
		sendManyCommand,
		sendCoinsCommand,
		connectCommand,
		openChannelCommand,
		closeChannelCommand,
		listPeersCommand,
		walletBalanceCommand,
		channelBalanceCommand,
		getInfoCommand,
		pendingChannelsCommand,
		sendPaymentCommand,
		addInvoiceCommand,
		lookupInvoiceCommand,
		listInvoicesCommand,
		listChannelsCommand,
		listPaymentsCommand,
		describeGraphCommand,
		getChanInfoCommand,
		getNodeInfoCommand,
		queryRoutesCommand,
		getNetworkInfoCommand,
		debugLevelCommand,
		decodePayReqComamnd,
		listChainTxnsCommand,
	}

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}
}
