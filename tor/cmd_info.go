package tor

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrServiceNotCreated is used when we want to query info on an onion
	// service while it's not been created yet.
	ErrServiceNotCreated = errors.New("onion service hasn't been created")

	// ErrServiceIDMismatch is used when the serviceID the controller has
	// doesn't match the serviceID the Tor daemon has.
	ErrServiceIDMismatch = errors.New("onion serviceIDs don't match")

	// ErrNoServiceFound is used when the Tor daemon replies no active
	// onion services found for the current control connection while we
	// expect one.
	ErrNoServiceFound = errors.New("no active service found")
)

// CheckOnionService checks that all onion services created by the controller
// are active. It queries the Tor daemon using the endpoint "onions/current" to
// get the current onion services and checks that their exact set matches every
// active service tracked by the controller.
func (c *Controller) CheckOnionService() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	expectedIDs, expectedSet := c.trackedServiceIDs()
	if len(expectedIDs) == 0 {
		return ErrServiceNotCreated
	}

	// Fetch the onion services that live in current control connection.
	cmd := "GETINFO onions/current"
	code, reply, err := c.sendCommandLocked(cmd)

	// Exit early if we got an error or Tor daemon didn't respond success.
	// TODO(yy): unify the usage of err and code so we could rely on a
	// single source to change our state.
	if err != nil || code != success {
		log.Debugf("query services got err:%v, reply:%v", err, reply)

		return fmt.Errorf("%w: %v", err, reply)
	}

	// Parse the comma-separated service IDs from onions/current.
	resp := parseTorReply(reply)
	serviceID, ok := resp["onions/current"]
	if !ok {
		return ErrNoServiceFound
	}

	if serviceID == "" {
		return ErrNoServiceFound
	}

	actualIDs := strings.Split(serviceID, ",")
	actualSet := make(map[string]struct{}, len(actualIDs))
	for _, actualID := range actualIDs {
		if actualID == "" {
			return serviceIDMismatch(expectedIDs, actualIDs)
		}
		if _, duplicate := actualSet[actualID]; duplicate {
			return serviceIDMismatch(expectedIDs, actualIDs)
		}

		actualSet[actualID] = struct{}{}
		if _, expected := expectedSet[actualID]; !expected {
			return serviceIDMismatch(expectedIDs, actualIDs)
		}
	}

	if len(actualSet) != len(expectedSet) {
		return serviceIDMismatch(expectedIDs, actualIDs)
	}

	return nil
}

// trackedServiceIDs returns every registered identity in deterministic order.
// The active set fallback supports controllers constructed before registration
// tracking and tests that exercise the control response parser directly.
func (c *Controller) trackedServiceIDs() ([]string, map[string]struct{}) {
	expectedIDs := make([]string, 0, len(c.registrations))
	expectedSet := make(map[string]struct{}, len(c.registrations))
	for _, registration := range c.registrations {
		serviceID := registration.serviceID
		if _, duplicate := expectedSet[serviceID]; duplicate {
			continue
		}

		expectedIDs = append(expectedIDs, serviceID)
		expectedSet[serviceID] = struct{}{}
	}

	if len(expectedIDs) != 0 {
		return expectedIDs, expectedSet
	}

	var remaining []string
	for serviceID := range c.activeServiceIDs {
		remaining = append(remaining, serviceID)
	}
	sort.Strings(remaining)
	for _, serviceID := range remaining {
		expectedSet[serviceID] = struct{}{}
	}

	return remaining, expectedSet
}

// serviceIDMismatch constructs a deterministic service set mismatch error.
func serviceIDMismatch(expectedIDs, actualIDs []string) error {
	return fmt.Errorf("%w: controller has: %v, Tor daemon has: %v",
		ErrServiceIDMismatch, expectedIDs, actualIDs)
}
