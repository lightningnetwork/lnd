package lncfg

import "errors"

// WtClient holds the configuration options for the daemon's watchtower client.
type WtClient struct {
	// Active determines whether a watchtower client should be created to
	// back up channel states with registered watchtowers.
	Active bool `long:"active" description:"Whether the daemon should use private watchtowers to back up revoked channel states."`

	// PrivateTowerURIs specifies the lightning URIs of the towers the
	// watchtower client should send new backups to.
	PrivateTowerURIs []string `long:"private-tower-uris" description:"(Deprecated) Specifies the URIs of private watchtowers to use in backing up revoked states. URIs must be of the form <pubkey>@<addr>. Only 1 URI is supported at this time, if none are provided the tower will not be enabled."`

	// SweepFeeRate specifies the fee rate in sat/byte to be used when
	// constructing justice transactions sent to the tower.
	SweepFeeRate uint64 `long:"sweep-fee-rate" description:"Specifies the fee rate in sat/byte to be used when constructing justice transactions sent to the watchtower."`
}

// Validate ensures the user has provided a valid configuration.
//
// NOTE: Part of the Validator interface.
func (c *WtClient) Validate() error {
	if len(c.PrivateTowerURIs) > 0 {
		return errors.New("`wtclient.private-tower-uris` is " +
			"deprecated and will be removed in the v0.8.0 " +
			"release, to specify watchtowers remove " +
			"`wtclient.private-tower-uris`, set " +
			"`wtclient.active`, and check out `lncli wtclient -h` " +
			"for more information on how to manage towers")
	}

	return nil
}

// Compile-time constraint to ensure WtClient implements the Validator
// interface.
var _ Validator = (*WtClient)(nil)
