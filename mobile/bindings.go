//go:build mobile
// +build mobile

package lndmobile

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	flags "github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/signal"
	_ "golang.org/x/mobile/bind"
	"google.golang.org/grpc"
)

// lndStarted will be used atomically to ensure only a single lnd instance is
// attempted to be started at once.
var lndStarted int32

// Start starts lnd in a new goroutine.
//
// extraArgs can be used to pass command line arguments to lnd that will
// override what is found in the config file. Example:
//
//	extraArgs = "--bitcoin.testnet --lnddir=\"/tmp/folder name/\" --profile=5050"
//
// The rpcReady is called lnd is ready to accept RPC calls.
//
// NOTE: On mobile platforms the '--lnddir` argument should be set to the
// current app directory in order to ensure lnd has the permissions needed to
// write to it.
func Start(extraArgs string, rpcReady Callback) {
	// We only support a single lnd instance at a time (singleton) for now,
	// so we make sure to return immediately if it has already been
	// started.
	if !atomic.CompareAndSwapInt32(&lndStarted, 0, 1) {
		err := errors.New("lnd already started")
		rpcReady.OnError(err)
		return
	}

	// (Re-)initialize the in-mem gRPC listeners we're going to give to lnd.
	// This is required each time lnd is started, because when lnd shuts
	// down, the in-mem listeners are closed.
	RecreateListeners()

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

	// Hook interceptor for os signals.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		atomic.StoreInt32(&lndStarted, 0)
		_, _ = fmt.Fprintln(os.Stderr, err)
		rpcReady.OnError(err)
		return
	}

	// Load the configuration, and parse the extra arguments as command
	// line options. This function will also set up logging properly.
	loadedConfig, err := lnd.LoadConfig(shutdownInterceptor)
	if err != nil {
		atomic.StoreInt32(&lndStarted, 0)
		_, _ = fmt.Fprintln(os.Stderr, err)
		rpcReady.OnError(err)
		return
	}

	// Set a channel that will be notified when the RPC server is ready to
	// accept calls.
	var (
		rpcListening = make(chan struct{})
		quit         = make(chan struct{})
	)

	// We call the main method with the custom in-memory listener called by
	// the mobile APIs, such that the grpc server will use it.
	cfg := lnd.ListenerCfg{
		RPCListeners: []*lnd.ListenerWithSignal{{
			Listener: lightningLis,
			Ready:    rpcListening,
		}},
	}
	implCfg := loadedConfig.ImplementationConfig(shutdownInterceptor)

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	go func() {
		defer atomic.StoreInt32(&lndStarted, 0)
		defer close(quit)

		if err := lnd.Main(
			loadedConfig, cfg, implCfg, shutdownInterceptor,
		); err != nil {
			if e, ok := err.(*flags.Error); ok &&
				e.Type == flags.ErrHelp {
			} else {
				fmt.Fprintln(os.Stderr, err)
			}
			rpcReady.OnError(err)
			return
		}
	}()

	// By default we'll apply the admin auth options, which will include
	// macaroons.
	setDefaultDialOption(
		func() ([]grpc.DialOption, error) {
			return lnd.AdminAuthOptions(loadedConfig, false)
		},
	)

	// For the WalletUnlocker and StateService, the macaroons might not be
	// available yet when called, so we use a more restricted set of
	// options that don't include them.
	setWalletUnlockerDialOption(
		func() ([]grpc.DialOption, error) {
			return lnd.AdminAuthOptions(loadedConfig, true)
		},
	)
	setStateDialOption(
		func() ([]grpc.DialOption, error) {
			return lnd.AdminAuthOptions(loadedConfig, true)
		},
	)

	// Finally we start a go routine that will call the provided callback
	// when the RPC server is ready to accept calls.
	go func() {
		select {
		case <-rpcListening:
		case <-quit:
			return
		}

		rpcReady.OnResponse([]byte{})
	}()
}
