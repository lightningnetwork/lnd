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
//
// The unlockerReady callback is called when the WalletUnlocker service is
// ready, and rpcReady is called after the wallet has been unlocked and lnd is
// ready to accept RPC calls.
func Start(extraArgs string, unlockerReady, rpcReady Callback) {
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

	// Set up channels that will be notified when the RPC servers are ready
	// to accept calls.
	var (
		unlockerListening = make(chan struct{})
		rpcListening      = make(chan struct{})
	)

	// We call the main method with the custom in-memory listeners called
	// by the mobile APIs, such that the grpc server will use these.
	cfg := lnd.ListenerCfg{
		WalletUnlocker: &lnd.ListenerWithSignal{
			Listener: walletUnlockerLis,
			Ready:    unlockerListening,
		},
		RPCListener: &lnd.ListenerWithSignal{
			Listener: lightningLis,
			Ready:    rpcListening,
		},
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

	// Finally we start two go routines that will call the provided
	// callbacks when the RPC servers are ready to accept calls.
	go func() {
		<-unlockerListening
		unlockerReady.OnResponse([]byte{})
	}()

	go func() {
		<-rpcListening

		// Now that the RPC server is ready, we can get the needed
		// authentication options, and add them to the global dial
		// options.
		auth, err := lnd.AdminAuthOptions()
		if err != nil {
			rpcReady.OnError(err)
			return
		}

		// Add the auth options to the listener's dial options.
		addLightningLisDialOption(auth...)

		rpcReady.OnResponse([]byte{})
	}()
}
