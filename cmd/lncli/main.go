package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/macaroon.v1"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/roasbeef/btcutil"
	"github.com/urfave/cli"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defaultTLSCertFilename  = "tls.cert"
	defaultMacaroonFilename = "admin.macaroon"
)

var (
	lndHomeDir          = btcutil.AppDataDir("lnd", false)
	defaultTLSCertPath  = filepath.Join(lndHomeDir, defaultTLSCertFilename)
	defaultMacaroonPath = filepath.Join(lndHomeDir, defaultMacaroonFilename)
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[lncli] %v\n", err)
	os.Exit(1)
}

func getWalletUnlockerClient(ctx *cli.Context) (lnrpc.WalletUnlockerClient, func()) {
	conn := getClientConn(ctx)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewWalletUnlockerClient(conn), cleanUp
}

func getClient(ctx *cli.Context) (lnrpc.LightningClient, func()) {
	conn := getClientConn(ctx)

	cleanUp := func() {
		conn.Close()
	}

	return lnrpc.NewLightningClient(conn), cleanUp
}

func getClientConn(ctx *cli.Context) *grpc.ClientConn {
	// Load the specified TLS certificate and build transport credentials
	// with it.
	tlsCertPath := cleanAndExpandPath(ctx.GlobalString("tlscertpath"))
	creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		fatal(err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Only process macaroon credentials if --no-macaroons isn't set.
	if !ctx.GlobalBool("no-macaroons") {
		// Load the specified macaroon file.
		macPath := cleanAndExpandPath(ctx.GlobalString("macaroonpath"))
		macBytes, err := ioutil.ReadFile(macPath)
		if err != nil {
			fatal(err)
		}
		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			fatal(err)
		}

		macConstraints := []macaroons.Constraint{
			// We add a time-based constraint to prevent replay of the
			// macaroon. It's good for 60 seconds by default to make up for
			// any discrepancy between client and server clocks, but leaking
			// the macaroon before it becomes invalid makes it possible for
			// an attacker to reuse the macaroon. In addition, the validity
			// time of the macaroon is extended by the time the server clock
			// is behind the client clock, or shortened by the time the
			// server clock is ahead of the client clock (or invalid
			// altogether if, in the latter case, this time is more than 60
			// seconds).
			// TODO(aakselrod): add better anti-replay protection.
			macaroons.TimeoutConstraint(ctx.GlobalInt64("macaroontimeout")),

			// Lock macaroon down to a specific IP address.
			macaroons.IPLockConstraint(ctx.GlobalString("macaroonip")),

			// ... Add more constraints if needed.
		}

		// Apply constraints to the macaroon.
		constrainedMac, err := macaroons.AddConstraints(mac, macConstraints...)
		if err != nil {
			fatal(err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred := macaroons.NewMacaroonCredential(constrainedMac)
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	conn, err := grpc.Dial(ctx.GlobalString("rpcserver"), opts...)
	if err != nil {
		fatal(err)
	}

	return conn
}

func main() {
	app := cli.NewApp()
	app.Name = "lncli"
	app.Version = "0.3"
	app.Usage = "control plane for your Lightning Network Daemon (lnd)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:10009",
			Usage: "host:port of ln daemon",
		},
		cli.StringFlag{
			Name:  "tlscertpath",
			Value: defaultTLSCertPath,
			Usage: "path to TLS certificate",
		},
		cli.BoolFlag{
			Name:  "no-macaroons",
			Usage: "disable macaroon authentication",
		},
		cli.StringFlag{
			Name:  "macaroonpath",
			Value: defaultMacaroonPath,
			Usage: "path to macaroon file",
		},
		cli.Int64Flag{
			Name:  "macaroontimeout",
			Value: 60,
			Usage: "anti-replay macaroon validity time in seconds",
		},
		cli.StringFlag{
			Name:  "macaroonip",
			Usage: "if set, lock macaroon to specific IP address",
		},
	}
	app.Commands = []cli.Command{
		createCommand,
		unlockCommand,
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
		payInvoiceCommand,
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
		feeReportCommand,
		updateFeesCommand,
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
