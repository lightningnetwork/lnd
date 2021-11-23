package healthcheck

import (
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/lightningnetwork/lnd/tor"
)

// CheckTorServiceStatus checks whether the onion service is reachable by
// sending a GETINFO command to the Tor daemon using our tor controller.
// We will get an EOF or a broken pipe error if the Tor daemon is
// stopped/restarted as the previously created socket connection is no longer
// open. In this case, we will attempt a restart on our tor controller. If the
// tor daemon comes back, a new socket connection will then be created.
func CheckTorServiceStatus(tc *tor.Controller,
	createService func() error) error {

	// Send a cmd using GETINFO onions/current and checks that our known
	// onion serviceID can be found.
	err := tc.CheckOnionService()
	if err == nil {
		return nil
	}

	log.Debugf("Checking tor service got: %v", err)

	switch {
	// We will get an EOF if the connection is lost. In this case, we will
	// return an error and wait for the Tor daemon to come back. We won't
	// attempt to make a new connection since we know Tor daemon is down.
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("Tor daemon connection lost, " +
			"check if Tor is up and running")

	// Once Tor daemon is down, we will get a broken pipe error when we use
	// the existing connection to make a GETINFO request since that socket
	// has now been closed. As Tor daemon might not be running yet, we will
	// attempt to make a new connection till Tor daemon is back.
	case errors.Is(err, syscall.EPIPE):
		log.Warnf("Tor connection lost, attempting a tor controller " +
			"re-connection...")

		// If the restart fails, we will attempt again during our next
		// healthcheck cycle.
		return restartTorController(tc, createService)

	// If this is not a connection layer error, such as
	// ErrServiceNotCreated or ErrServiceIDMismatch, there's little we can
	// do but to report the error to the user.
	default:
		return err
	}
}

// restartTorController attempts to make a new connection to the Tor daemon and
// re-create the Hidden Service.
func restartTorController(tc *tor.Controller,
	createService func() error) error {

	err := tc.Reconnect()

	// If we get a connection refused error, it means Tor daemon might not
	// be started.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check if Tor daemon is running")
	}

	// Otherwise, we get an unexpected and return it.
	if err != nil {
		log.Errorf("Re-connectting tor got err: %v", err)
		return err
	}

	// Recreate the Hidden Service.
	if err := createService(); err != nil {
		log.Errorf("Re-create service tor got err: %v", err)
		return err
	}

	log.Info("Successfully restarted tor connection!")

	return nil
}
