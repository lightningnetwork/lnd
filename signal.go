package main

// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/signal.go

import (
	"os"
	"os/signal"
)

// interruptChannel is used to receive SIGINT (Ctrl+C) signals.
var interruptChannel chan os.Signal

// addHandlerChannel is used to add an interrupt handler to the list of handlers
// to be invoked on SIGINT (Ctrl+C) signals.
var addHandlerChannel = make(chan func())

// mainInterruptHandler listens for SIGINT (Ctrl+C) signals on the
// interruptChannel and invokes the registered interruptCallbacks accordingly.
// It also listens for callback registration.  It must be run as a goroutine.
func mainInterruptHandler() {
	// interruptCallbacks is a list of callbacks to invoke when a
	// SIGINT (Ctrl+C) is received.
	var interruptCallbacks []func()

	// isShutdown is a flag which is used to indicate whether or not
	// the shutdown signal has already been received and hence any future
	// attempts to add a new interrupt handler should invoke them
	// immediately.
	var isShutdown bool

	for {
		select {
		case <-interruptChannel:
			// Ignore more than one shutdown signal.
			if isShutdown {
				ltndLog.Infof("Received SIGINT (Ctrl+C).  " +
					"Already shutting down...")
				continue
			}

			isShutdown = true
			ltndLog.Infof("Received SIGINT (Ctrl+C).  Shutting down...")

			// Run handlers in LIFO order.
			for i := range interruptCallbacks {
				idx := len(interruptCallbacks) - 1 - i
				callback := interruptCallbacks[idx]
				callback()
			}

			// Signal the main goroutine to shutdown.
			go func() {
				shutdownChannel <- struct{}{}
			}()

		case handler := <-addHandlerChannel:
			// The shutdown signal has already been received, so
			// just invoke and new handlers immediately.
			if isShutdown {
				handler()
			}

			interruptCallbacks = append(interruptCallbacks, handler)
		}
	}
}

// addInterruptHandler adds a handler to call when a SIGINT (Ctrl+C) is
// received.
func addInterruptHandler(handler func()) {
	// Create the channel and start the main interrupt handler which invokes
	// all other callbacks and exits if not already done.
	if interruptChannel == nil {
		interruptChannel = make(chan os.Signal, 1)
		signal.Notify(interruptChannel, os.Interrupt)
		go mainInterruptHandler()
	}

	addHandlerChannel <- handler
}
