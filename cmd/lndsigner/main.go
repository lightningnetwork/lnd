package main

import (
	"errors"
	"fmt"
	"os"

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

	// Load the signer configuration, and parse any command line options.
	// This function will also set up logging properly.
	loadedConfig, err := lnd.LoadSignerConfig(shutdownInterceptor)
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			// Help was requested, exit normally.
			os.Exit(0)
		}

		// Print error if not due to help request.
		err = fmt.Errorf("failed to load config: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	implCfg := loadedConfig.ImplementationConfig(shutdownInterceptor)

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	if err = lnd.Main(
		loadedConfig, lnd.ListenerCfg{}, implCfg, shutdownInterceptor,
	); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
