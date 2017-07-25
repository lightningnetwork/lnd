package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcutil"
	"github.com/urfave/cli"

	flags "github.com/btcsuite/go-flags"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defaultConfigFilename  = "lnd.conf"
	defaultTLSCertFilename = "tls.cert"
)

var (
	lndHomeDir         = btcutil.AppDataDir("lnd", false)
	defaultConfigFile  = filepath.Join(lndHomeDir, defaultConfigFilename)
	defaultTLSCertPath = filepath.Join(lndHomeDir, defaultTLSCertFilename)
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

type config struct {
	TLSCertPath string `long:"tlscertpath" description:"path to TLS certificate"`
}

func getClientConn(ctx *cli.Context) *grpc.ClientConn {
	// TODO(roasbeef): macaroon based auth
	// * http://www.grpc.io/docs/guides/auth.html
	// * http://research.google.com/pubs/pub41892.html
	// * https://github.com/go-macaroon/macaroon
	cfg := config{
		TLSCertPath: defaultTLSCertPath,
	}

	// We want only the TLS certificate information from the configuration
	// file at this time, so ignore anything else. We can always add fields
	// as we need them. When specifying a file on the `lncli` command line,
	// this should work with just a trusted CA cert assuming the server's
	// cert file contains the entire chain from the CA to the server's cert.
	parser := flags.NewParser(&cfg, flags.IgnoreUnknown)
	iniParser := flags.NewIniParser(parser)
	if err := iniParser.ParseFile(ctx.GlobalString("config")); err != nil {
		fatal(err)
	}
	if ctx.GlobalString("tlscertpath") != defaultTLSCertPath {
		cfg.TLSCertPath = ctx.GlobalString("tlscertpath")
	}
	cfg.TLSCertPath = cleanAndExpandPath(cfg.TLSCertPath)
	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		fatal(err)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	conn, err := grpc.Dial(ctx.GlobalString("rpcserver"), opts...)
	if err != nil {
		fatal(err)
	}

	return conn
}

func main() {
	app := cli.NewApp()
	app.Name = "lncli"
	app.Version = "0.2"
	app.Usage = "control plane for your Lightning Network Daemon (lnd)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:10009",
			Usage: "host:port of ln daemon",
		},
		cli.StringFlag{
			Name:  "config",
			Value: defaultConfigFile,
			Usage: "path to config file for TLS cert path",
		},
		cli.StringFlag{
			Name:  "tlscertpath",
			Value: defaultTLSCertPath,
			Usage: "path to TLS certificate",
		},
	}
	app.Commands = []cli.Command{
		newAddressCommand,
		sendManyCommand,
		sendCoinsCommand,
		connectCommand,
		disconnectCommand,
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
		stopCommand,
		signMessageCommand,
		verifyMessageCommand,
	}

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		homeDir := filepath.Dir(lndHomeDir)
		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}
