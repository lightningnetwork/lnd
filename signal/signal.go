// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/signal.go
// Copyright (C) 2015-2022 The Lightning Network Developers

package signal

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/coreos/go-systemd/daemon"
)

var (
	// started indicates whether we have started our main interrupt handler.
	// This field should be used atomically.
	started int32
)

// systemdNotifyReady notifies systemd about LND being ready, logs the result of
// the operation or possible error. Besides logging, systemd being unavailable
// is ignored.
func systemdNotifyReady() error {
	notified, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		err := fmt.Errorf("failed to notify systemd %v (if you aren't "+
			"running systemd clear the environment variable "+
			"NOTIFY_SOCKET)", err)
		log.Error(err)

		// The SdNotify doc says it's common to ignore the
		// error. We don't want to ignore it because if someone
		// set up systemd to wait for initialization other
		// processes would get stuck.
		return err
	}
	if notified {
		log.Info("Systemd was notified about our readiness")
	} else {
		log.Info("We're not running within systemd or the service " +
			"type is not 'notify'")
	}
	return nil
}

// systemdNotifyStop notifies systemd that LND is stopping and logs error if
// the notification failed. It also logs if the notification was actually sent.
// Systemd being unavailable is intentionally ignored.
func systemdNotifyStop() {
	notified, err := daemon.SdNotify(false, daemon.SdNotifyStopping)

	// Just log - we're stopping anyway.
	if err != nil {
		log.Errorf("Failed to notify systemd: %v", err)
	}
	if notified {
		log.Infof("Systemd was notified about stopping")
	}
}

// Notifier handles notifications about status of LND.
type Notifier struct {
	// notifiedReady remembers whether Ready was sent to avoid sending it
	// multiple times.
	notifiedReady bool
}

// NotifyReady notifies other applications that RPC is ready.
func (notifier *Notifier) NotifyReady(walletUnlocked bool) error {
	if !notifier.notifiedReady {
		err := systemdNotifyReady()
		if err != nil {
			return err
		}
		notifier.notifiedReady = true
	}
	if walletUnlocked {
		_, _ = daemon.SdNotify(false, "STATUS=Wallet unlocked")
	} else {
		_, _ = daemon.SdNotify(false, "STATUS=Wallet locked")
	}

	return nil
}

// notifyStop notifies other applications that LND is stopping.
func (notifier *Notifier) notifyStop() {
	systemdNotifyStop()
}

// Interceptor contains channels and methods regarding application shutdown
// and interrupt signals.
type Interceptor struct {
	// interruptChannel is used to receive SIGINT (Ctrl+C) signals.
	interruptChannel chan os.Signal

	// shutdownChannel is closed once the main interrupt handler exits.
	shutdownChannel chan struct{}

	// shutdownRequestChannel is used to request the daemon to shutdown
	// gracefully, similar to when receiving SIGINT.
	shutdownRequestChannel chan struct{}

	// quit is closed when instructing the main interrupt handler to exit.
	// Note that to avoid losing notifications, only shutdown func may
	// close this channel.
	quit chan struct{}

	// exitCode is the code the process should exit with once shutdown
	// completes. It is a pointer so that every copy of the interceptor
	// shares it, as the interceptor is passed around by value.
	exitCode *atomic.Int32

	// Notifier handles sending shutdown notifications.
	Notifier Notifier
}

// Intercept starts the interception of interrupt signals and returns an `Interceptor` instance.
// Note that any previous active interceptor must be stopped before a new one can be created.
func Intercept() (Interceptor, error) {
	if !atomic.CompareAndSwapInt32(&started, 0, 1) {
		return Interceptor{}, errors.New("intercept already started")
	}

	channels := Interceptor{
		interruptChannel:       make(chan os.Signal, 1),
		shutdownChannel:        make(chan struct{}),
		shutdownRequestChannel: make(chan struct{}),
		quit:                   make(chan struct{}),
		exitCode:               &atomic.Int32{},
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
		c.Notifier.notifyStop()

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

			// Release the started flag before signaling the exit
			// so a new interceptor can be started as soon as the
			// shutdown channel is closed.
			atomic.StoreInt32(&started, 0)
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

// RequestShutdown initiates a graceful shutdown from the application. Unless
// an exit code was recorded with SetExitCode, the process exits with
// ExitCodeSuccess.
func (c *Interceptor) RequestShutdown() {
	select {
	case c.shutdownRequestChannel <- struct{}{}:
	case <-c.quit:
	}
}

// SetExitCode records the code the process should exit with once shutdown
// completes. Only the first non-zero code is kept, so that the specific
// reason recorded by the component that detected a failure is not overwritten
// by the generic critical error raised while logging it. A code recorded
// after shutdown has already begun is ignored, so a failure noticed during
// the teardown of a deliberate stop does not turn it into a failed exit. That
// guard is best-effort: the liveness check and the record are separate steps,
// so a code recorded in the instant shutdown begins may still be kept.
func (c *Interceptor) SetExitCode(code ExitCode) {
	if c.exitCode == nil || !c.Alive() {
		return
	}

	c.exitCode.CompareAndSwap(int32(ExitCodeSuccess), int32(code))
}

// ExitCode returns the code the process should exit with. It is
// ExitCodeSuccess unless a failure was recorded with SetExitCode.
func (c *Interceptor) ExitCode() ExitCode {
	if c.exitCode == nil {
		return ExitCodeSuccess
	}

	return ExitCode(c.exitCode.Load())
}

// ShutdownChannel returns the channel that will be closed once the main
// interrupt handler has exited.
func (c *Interceptor) ShutdownChannel() <-chan struct{} {
	return c.shutdownChannel
}
