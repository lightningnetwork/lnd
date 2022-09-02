//go:build bolt12

package main

import (
	"fmt"
	"os"

	"github.com/gijswijs/boltnd"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/signal"
)

func main() {
	// Hook interceptor for os signals.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := lnd.LoadConfig(shutdownInterceptor)
	if err != nil {
		if e, ok := err.(*flags.Error); !ok || e.Type != flags.ErrHelp {
			// Print error if not due to help request.
			err = fmt.Errorf("failed to load config: %w", err)
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		// Help was requested, exit normally.
		os.Exit(0)
	}

	// Create a new, external bolt implementation using our config and
	// logger.
	boltImpl, err := boltnd.NewBoltnd(
		boltnd.OptionLNDConfig(loadedConfig),
		boltnd.OptionLNDLogger(
			loadedConfig.SubLogMgr, shutdownInterceptor,
		),
		boltnd.OptionRequestShutdown(
			shutdownInterceptor.RequestShutdown,
		),
	)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// TODO(carla): figure out defaults/ whether we need this?
	implCfg := loadedConfig.ImplementationConfig(shutdownInterceptor)

	// Add our external macaroon validator and grpc registrar.
	implCfg.GrpcRegistrar = boltImpl
	implCfg.ExternalValidator = boltImpl

	// We want lnd to start up before we start our boltnd implementation,
	// because it needs to connect to the API. We start lnd in a goroutine
	// using the done channel to indicate when lnd has exited.
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Call the "real" main in a nested manner so the defers will
		// properly be executed in the case of a graceful shutdown.
		if err = lnd.Main(
			loadedConfig, lnd.ListenerCfg{}, implCfg,
			shutdownInterceptor,
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()

	// Start our external bolt impl. If our start fails, we do not exit but
	// rather request shutdown from lnd and allow it the time to cleanly
	// exit.
	if err := boltImpl.Start(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "boltnd start", err)
		shutdownInterceptor.RequestShutdown()
	}

	// Once the done channel is closed, we know that lnd has exited, either
	// from an internal error or on request from boltnd (because it
	// encountered a critical error.
	<-done

	// Finally, shut down our external implementation.
	if err := boltImpl.Stop(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "boltnd stop",
			err)
		os.Exit(1)
	}

}
