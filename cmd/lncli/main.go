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

func getClient(ctx *cli.Context) lnrpc.LightningClient {
	conn := getClientConn(ctx)
	return lnrpc.NewLightningClient(conn)
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
		NewAddressCommand,
		SendManyCommand,
		SendCoinsCommand,
		ConnectCommand,
		OpenChannelCommand,
		CloseChannelCommand,
		ListPeersCommand,
		WalletBalanceCommand,
		ChannelBalanceCommand,
		GetInfoCommand,
		PendingChannelsCommand,
		SendPaymentCommand,
		AddInvoiceCommand,
		LookupInvoiceCommand,
		ListInvoicesCommand,
		ListChannelsCommand,
		ListPaymentsCommand,
		DescribeGraphCommand,
		GetChanInfoCommand,
		GetNodeInfoCommand,
		QueryRouteCommand,
		GetNetworkInfoCommand,
	}

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}
}
