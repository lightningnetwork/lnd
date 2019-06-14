package lncfg

import (
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/lnwire"
)

// WtClient holds the configuration options for the daemon's watchtower client.
type WtClient struct {
	// PrivateTowerURIs specifies the lightning URIs of the towers the
	// watchtower client should send new backups to.
	PrivateTowerURIs []string `long:"private-tower-uris" description:"Specifies the URIs of private watchtowers to use in backing up revoked states. URIs must be of the form <pubkey>@<addr>. Only 1 URI is supported at this time, if none are provided the tower will not be enabled."`

	// PrivateTowers is the list of towers parsed from the URIs provided in
	// PrivateTowerURIs.
	PrivateTowers []*lnwire.NetAddress

	// SweepFeeRate specifies the fee rate in sat/byte to be used when
	// constructing justice transactions sent to the tower.
	SweepFeeRate uint64 `long:"sweep-fee-rate" description:"Specifies the fee rate in sat/byte to be used when constructing justice transactions sent to the watchtower."`
}

// Validate asserts that at most 1 private watchtower is requested.
//
// NOTE: Part of the Validator interface.
func (c *WtClient) Validate() error {
	if len(c.PrivateTowerURIs) > 1 {
		return fmt.Errorf("at most 1 private watchtower is supported, "+
			"found %d", len(c.PrivateTowerURIs))
	}

	return nil
}

// IsActive returns true if the watchtower client should be active.
func (c *WtClient) IsActive() bool {
	return len(c.PrivateTowerURIs) > 0
}

// ParsePrivateTowers parses any private tower URIs held PrivateTowerURIs. The
// value of port should be the default port to use when a URI does not have one.
func (c *WtClient) ParsePrivateTowers(port int, resolver TCPResolver) error {
	towers := make([]*lnwire.NetAddress, 0, len(c.PrivateTowerURIs))
	for _, uri := range c.PrivateTowerURIs {
		addr, err := ParseLNAddressString(
			uri, strconv.Itoa(port), resolver,
		)
		if err != nil {
			return fmt.Errorf("unable to parse private "+
				"watchtower address: %v", err)
		}

		towers = append(towers, addr)
	}

	c.PrivateTowers = towers

	return nil
}

// Compile-time constraint to ensure WtClient implements the Validator
// interface.
var _ Validator = (*WtClient)(nil)
