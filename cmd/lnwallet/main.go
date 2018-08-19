// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2018 The Lightning Network Developers

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/urfave/cli"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	//Commit stores the current commit hash of this build. This should be
	//set using -ldflags during compilation.
	Commit string

	errNoConsole = errors.New("wallet db requires console access")
)

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "[lnwallet] %v\n", err)
	os.Exit(1)
}

func getNetParams(ctx *cli.Context) (*chaincfg.Params, uint32) {
	if ctx.GlobalBool("testnet") {
		return &chaincfg.TestNet3Params, keychain.CoinTypeTestnet
	}
	return &chaincfg.MainNetParams, keychain.CoinTypeBitcoin
}

func readPassword(ctx *cli.Context, userQuery string) []byte {
	// Parameter is set.
	if ctx.GlobalIsSet("password") {
		return []byte(ctx.GlobalString("password"))
	}

	// Read from terminal (if there is one).
	if terminal.IsTerminal(syscall.Stdin) {
		fmt.Printf(userQuery)
		pw, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			fatal(err)
		}
		fmt.Println()
		return pw
	}

	// Read from stdin as a fallback.
	reader := bufio.NewReader(os.Stdin)
	pw, err := reader.ReadBytes('\n')
	if err != nil {
		fatal(err)
	}
	return pw
}

func noConsole() ([]byte, error) {
	return nil, errNoConsole
}

func main() {
	app := cli.NewApp()
	app.Name = "lnwallet"
	app.Version = build.Version()
	app.Usage = "wallet utility for your Lightning Network Daemon (lnd)"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "testnet",
			Usage: "use testnet parameters",
		},
		cli.StringFlag{
			Name: "password",
			Usage: "wallet password as a command line parameter " +
				"(not recommended for security reasons!)",
		},
	}
	app.Commands = []cli.Command{
		dumpWalletCommand,
		walletInfoCommand,
	}

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	if path == "" {
		return ""
	}

	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		var homeDir string
		u, err := user.Current()
		if err == nil {
			homeDir = u.HomeDir
		} else {
			homeDir = os.Getenv("HOME")
		}

		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}
