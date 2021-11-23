package tor

import (
	"errors"
	"fmt"
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

// CheckOnionService checks that the onion service created by the controller
// is active. It queries the Tor daemon using the endpoint "onions/current" to
// get the current onion service and checks that service ID matches the
// activeServiceID.
func (c *Controller) CheckOnionService() error {
	// Check that we have a hidden service created.
	if c.activeServiceID == "" {
		return ErrServiceNotCreated
	}

	// Fetch the onion services that live in current control connection.
	cmd := "GETINFO onions/current"
	code, reply, err := c.sendCommand(cmd)

	// Exit early if we got an error or Tor daemon didn't respond success.
	// TODO(yy): unify the usage of err and code so we could rely on a
	// single source to change our state.
	if err != nil || code != success {
		log.Debugf("query service:%v got err:%v, reply:%v",
			c.activeServiceID, err, reply)

		return fmt.Errorf("%w: %v", err, reply)
	}

	// Parse the reply, which should have the following format,
	//      onions/current=serviceID
	// After parsing, we get a map as,
	// 	[onion/current: serviceID]
	//
	// NOTE: our current tor controller does NOT support multiple onion
	// services to be created at the same time, thus we expect the reply to
	// only contain one serviceID. If multiple serviceIDs are returned, we
	// would expected the reply to have the following format,
	//      onions/current=serviceID1, serviceID2, serviceID3,...
	// Thus a new parser is need to parse that reply.
	resp := parseTorReply(reply)
	serviceID, ok := resp["onions/current"]
	if !ok {
		return ErrNoServiceFound
	}

	// Check that our active service is indeed the service acknowledged by
	// Tor daemon. The controller is only aware of a single service but the
	// Tor daemon might have multiple services registered (for example for
	// the watchtower as well as the node p2p connections). So we just want
	// to check that our current controller's ID is contained in the list of
	// registered services.
	if !strings.Contains(serviceID, c.activeServiceID) {
		return fmt.Errorf("%w: controller has: %v, Tor daemon has: %v",
			ErrServiceIDMismatch, c.activeServiceID, serviceID)
	}

	return nil
}
