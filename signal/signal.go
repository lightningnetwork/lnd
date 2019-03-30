// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/signal.go
// Copyright (C) 2015-2017 The Lightning Network Developers

package signal

import (
	"os"
	"os/signal"
	"syscall"
)

var (
	// interruptChannel is used to receive SIGINT (Ctrl+C) signals.
	interruptChannel = make(chan os.Signal, 1)

	// shutdownRequestChannel is used to request the daemon to shutdown
	// gracefully, similar to when receiving SIGINT.
	shutdownRequestChannel = make(chan struct{})

	// quit is closed when instructing the main interrupt handler to exit.
	quit = make(chan struct{})

	// shutdownChannel is closed once the main interrupt handler exits.
	shutdownChannel = make(chan struct{})
)

func init() {
	signalsToCatch := []os.Signal{
		os.Interrupt,
		os.Kill,
		syscall.SIGABRT,
		syscall.SIGTERM,
		syscall.SIGSTOP,
		syscall.SIGQUIT,
	}
	signal.Notify(interruptChannel, signalsToCatch...)
	go mainInterruptHandler()
}

// mainInterruptHandler listens for SIGINT (Ctrl+C) signals on the
// interruptChannel and shutdown requests on the shutdownRequestChannel, and
// invokes the registered interruptCallbacks accordingly. It also listens for
// callback registration.
// It must be run as a goroutine.
func mainInterruptHandler() {
	// isShutdown is a flag which is used to indicate whether or not
	// the shutdown signal has already been received and hence any future
	// attempts to add a new interrupt handler should invoke them
	// immediately.
	var isShutdown bool

	// shutdown invokes the registered interrupt handlers, then signals the
	// shutdownChannel.
	shutdown := func() {
		// Ignore more than one shutdown signal.
		if isShutdown {
			log.Infof("Already shutting down...")
			return
		}
		isShutdown = true
		log.Infof("Shutting down...")

		// Signal the main interrupt handler to exit, and stop accept
		// post-facto requests.
		close(quit)
	}

	for {
		select {
		case signal := <-interruptChannel:
			log.Infof("Received %v", signal)
			shutdown()

		case <-shutdownRequestChannel:
			log.Infof("Received shutdown request.")
			shutdown()

		case <-quit:
			log.Infof("Gracefully shutting down.")
			close(shutdownChannel)
			return
		}
	}
}

// Alive returns true if the main interrupt handler has not been killed.
func Alive() bool {
	select {
	case <-quit:
		return false
	default:
		return true
	}
}

// RequestShutdown initiates a graceful shutdown from the application.
func RequestShutdown() {
	select {
	case shutdownRequestChannel <- struct{}{}:
	case <-quit:
	}
}

// ShutdownChannel returns the channel that will be closed once the main
// interrupt handler has exited.
func ShutdownChannel() <-chan struct{} {
	return shutdownChannel
}
