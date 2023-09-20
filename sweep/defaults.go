package sweep

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// DefaultBatchWindowDuration specifies duration of the sweep batch
	// window. The sweep is held back during the batch window to allow more
	// inputs to be added and thereby lower the fee per input.
	DefaultBatchWindowDuration = 30 * time.Second

	// DefaultMaxFeeRate is the default maximum fee rate allowed within the
	// UtxoSweeper. The current value is equivalent to a fee rate of 1,000
	// sat/vbyte.
	DefaultMaxFeeRate chainfee.SatPerVByte = 1e3
)
