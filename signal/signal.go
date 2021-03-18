// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/signal.go
// Copyright (C) 2015-2017 The Lightning Network Developers

package signal

import (
	"errors"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

var (
	// started indicates whether we have started our main interrupt handler.
	// This field should be used atomically.
	started int32
)

// Interceptor contains channels and methods regarding application shutdown
// and interrupt signals
type Interceptor struct {
	// interruptChannel is used to receive SIGINT (Ctrl+C) signals.
	interruptChannel chan os.Signal

	// shutdownChannel is closed once the main interrupt handler exits.
	shutdownChannel chan struct{}

	// shutdownRequestChannel is used to request the daemon to shutdown
	// gracefully, similar to when receiving SIGINT.
	shutdownRequestChannel chan struct{}

	// quit is closed when instructing the main interrupt handler to exit.
	quit chan struct{}
}

// Intercept starts the interception of interrupt signals and returns an `Interceptor` instance.
// Note that any previous active interceptor must be stopped before a new one can be created
func Intercept() (Interceptor, error) {
	if !atomic.CompareAndSwapInt32(&started, 0, 1) {
		return Interceptor{}, errors.New("intercept already started")
	}

	channels := Interceptor{
		interruptChannel:       make(chan os.Signal, 1),
		shutdownChannel:        make(chan struct{}),
		shutdownRequestChannel: make(chan struct{}),
		quit:                   make(chan struct{}),
	}

	signalsToCatch := []os.Signal{
		os.Interrupt,
		os.Kill,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	}
	signal.Notify(channels.interruptChannel, signalsToCatch...)
	go channels.mainInterruptHandler()

	return channels, nil
}

// mainInterruptHandler listens for SIGINT (Ctrl+C) signals on the
// interruptChannel and shutdown requests on the shutdownRequestChannel, and
// invokes the registered interruptCallbacks accordingly. It also listens for
// callback registration.
// It must be run as a goroutine.
func (c *Interceptor) mainInterruptHandler() {
	defer atomic.StoreInt32(&started, 0)
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
		close(c.quit)
	}

	for {
		select {
		case signal := <-c.interruptChannel:
			log.Infof("Received %v", signal)
			shutdown()

		case <-c.shutdownRequestChannel:
			log.Infof("Received shutdown request.")
			shutdown()

		case <-c.quit:
			log.Infof("Gracefully shutting down.")
			close(c.shutdownChannel)
			signal.Stop(c.interruptChannel)
			return
		}
	}
}

// Listening returns true if the main interrupt handler has been started, and
// has not been killed.
func (c *Interceptor) Listening() bool {
	// If our started field is not set, we are not yet listening for
	// interrupts.
	if atomic.LoadInt32(&started) != 1 {
		return false
	}

	// If we have started our main goroutine, we check whether we have
	// stopped it yet.
	return c.Alive()
}

// Alive returns true if the main interrupt handler has not been killed.
func (c *Interceptor) Alive() bool {
	select {
	case <-c.quit:
		return false
	default:
		return true
	}
}

// RequestShutdown initiates a graceful shutdown from the application.
func (c *Interceptor) RequestShutdown() {
	select {
	case c.shutdownRequestChannel <- struct{}{}:
	case <-c.quit:
	}
}

// ShutdownChannel returns the channel that will be closed once the main
// interrupt handler has exited.
func (c *Interceptor) ShutdownChannel() <-chan struct{} {
	return c.shutdownChannel
}
