package lncfg

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// DefaultMinUpdateTimeout represents the minimum interval in which a
// WebAPIEstimator will request fresh fees from its API.
const DefaultMinUpdateTimeout = 5 * time.Minute

// DefaultMaxUpdateTimeout represents the maximum interval in which a
// WebAPIEstimator will request fresh fees from its API.
const DefaultMaxUpdateTimeout = 20 * time.Minute

// DefaultMinRelayFeeRate is the default minimum relay fee rate floor in
// sat/vb. 1 sat/vb equals 250 sat/kw; the relay floor is set to 1 sat/vb
// so that the effective floor remains FeePerKwFloor (253 sat/kw) after
// the minFeeManager rounds up from the backend's reported value.
const DefaultMinRelayFeeRate chainfee.SatPerVByte = 1

// Fee holds the configuration options for fee estimation.
//
//nolint:ll
type Fee struct {
	URL              string               `long:"url" description:"Optional URL for external fee estimation. If no URL is specified, the method for fee estimation will depend on the chosen backend and network. Must be set for neutrino on mainnet."`
	MinUpdateTimeout time.Duration        `long:"min-update-timeout" description:"The minimum interval in which fees will be updated from the specified fee URL."`
	MaxUpdateTimeout time.Duration        `long:"max-update-timeout" description:"The maximum interval in which fees will be updated from the specified fee URL."`
	MinRelayFeeRate  chainfee.SatPerVByte `long:"min-relay-feerate" description:"Minimum fee rate floor in sat/vb used when estimating and enforcing on-chain fees. Lowering this below 1 sat/vb is only safe when your Bitcoin backend is configured with a matching minrelaytxfee and you control the path to miners. Default: 1 sat/vb (250 sat/kw)."`
}

// Validate checks the fee configuration for invalid values.
func (f *Fee) Validate() error {
	if f.MinRelayFeeRate < 0 {
		return fmt.Errorf("fee.min-relay-feerate must be >= 0")
	}

	return nil
}

// FeeFloorKW returns the configured minimum relay fee rate as sat/kw.
func (f *Fee) FeeFloorKW() chainfee.SatPerKWeight {
	return f.MinRelayFeeRate.FeePerKWeight()
}
