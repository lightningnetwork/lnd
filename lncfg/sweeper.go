package lncfg

import (
	"fmt"
	"time"
)

//nolint:lll
type Sweeper struct {
	BatchWindowDuration time.Duration `long:"batchwindowduration" description:"Duration of the sweep batch window. The sweep is held back during the batch window to allow more inputs to be added and thereby lower the fee per input."`
}

// Validate checks the values configured for the sweeper.
func (s *Sweeper) Validate() error {
	if s.BatchWindowDuration < 0 {
		return fmt.Errorf("batchwindowduration must be positive")
	}

	return nil
}
