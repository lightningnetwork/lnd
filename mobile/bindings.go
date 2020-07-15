// +build mobile

package lndmobile

import (
	"fmt"
	"os"
	"strings"

	flags "github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/signal"
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
//
// NOTE: On mobile platforms the '--lnddir` argument should be set to the
// current app directory in order to ensure lnd has the permissions needed to
// write to it.
func Start(extraArgs string, unlockerReady, rpcReady Callback) {
	// Split the argument string on "--" to get separated command line
	// arguments.
	var splitArgs []string
	for _, a := range strings.Split(extraArgs, "--") {
		// Trim any whitespace space, and ignore empty params.
		a := strings.TrimSpace(a)
		if a == "" {
			continue
		}

		// Finally we prefix any non-empty string with -- to mimic the
		// regular command line arguments.
		splitArgs = append(splitArgs, "--"+a)
	}

	// Add the extra arguments to os.Args, as that will be parsed in
	// LoadConfig below.
	os.Args = append(os.Args, splitArgs...)

	// Load the configuration, and parse the extra arguments as command
	// line options. This function will also set up logging properly.
	loadedConfig, err := lnd.LoadConfig()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Hook interceptor for os signals.
	signal.Intercept()

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
		if err := lnd.Main(
			loadedConfig, cfg, signal.ShutdownChannel(),
		); err != nil {
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

		// We must set the TLS certificates in order to properly
		// authenticate with the wallet unlocker service.
		auth, err := lnd.WalletUnlockerAuthOptions(loadedConfig)
		if err != nil {
			unlockerReady.OnError(err)
			return
		}

		// Add the auth options to the listener's dial options.
		addWalletUnlockerLisDialOption(auth...)

		unlockerReady.OnResponse([]byte{})
	}()

	go func() {
		<-rpcListening

		// Now that the RPC server is ready, we can get the needed
		// authentication options, and add them to the global dial
		// options.
		auth, err := lnd.AdminAuthOptions(loadedConfig)
		if err != nil {
			rpcReady.OnError(err)
			return
		}

		// Add the auth options to the listener's dial options.
		addLightningLisDialOption(auth...)

		rpcReady.OnResponse([]byte{})
	}()
}
