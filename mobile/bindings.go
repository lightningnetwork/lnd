// +build ios android

package lndmobile

import (
	"fmt"
	"os"
	"strings"

	flags "github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd"
)

// Start starts lnd in a new goroutine.
//
// extraArgs can be used to pass command line arguments to lnd that will
// override what is found in the config file. Example:
//	extraArgs = "--bitcoin.testnet --lnddir=\"/tmp/folder name/\" --profile=5050"
func Start(extraArgs string, callback Callback) {
	// Split the argument string on "--" to get separated command line
	// arguments.
	var splitArgs []string
	for _, a := range strings.Split(extraArgs, "--") {
		if a == "" {
			continue
		}
		// Finally we prefix any non-empty string with --, and trim
		// whitespace to mimic the regular command line arguments.
		splitArgs = append(splitArgs, strings.TrimSpace("--"+a))
	}

	// Add the extra arguments to os.Args, as that will be parsed during
	// startup.
	os.Args = append(os.Args, splitArgs...)

	// We call the main method with the custom in-memory listeners called
	// by the mobile APIs, such that the grpc server will use these.
	cfg := lnd.ListenerCfg{
		WalletUnlocker: walletUnlockerLis,
		RPCListener:    lightningLis,
	}

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	go func() {
		if err := lnd.Main(cfg); err != nil {
			if e, ok := err.(*flags.Error); ok &&
				e.Type == flags.ErrHelp {
			} else {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(1)
		}
	}()

	// TODO(halseth): callback when RPC server is actually running. Since
	// the RPC server might take a while to start up, the client might
	// assume it is ready to accept calls when this callback is sent, while
	// it's not.
	callback.OnResponse([]byte("started"))
}
