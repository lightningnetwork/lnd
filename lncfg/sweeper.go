package lncfg

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// MaxFeeRateFloor is the smallest config value allowed for the max fee rate in
// sat/vb.
const MaxFeeRateFloor = 100

//nolint:lll
type Sweeper struct {
	BatchWindowDuration time.Duration        `long:"batchwindowduration" description:"Duration of the sweep batch window. The sweep is held back during the batch window to allow more inputs to be added and thereby lower the fee per input."`
	MaxFeeRate          chainfee.SatPerVByte `long:"maxfeerate" description:"Maximum fee rate in sat/vb that the sweeper is allowed to use when sweeping funds."`
}

// Validate checks the values configured for the sweeper.
func (s *Sweeper) Validate() error {
	if s.BatchWindowDuration < 0 {
		return fmt.Errorf("batchwindowduration must be positive")
	}

	// We require the max fee rate to be at least 100 sat/vbyte.
	if s.MaxFeeRate < MaxFeeRateFloor {
		return fmt.Errorf("maxfeerate must be greater than 100 sat/vb")
	}

	return nil
}
