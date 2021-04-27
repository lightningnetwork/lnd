// +build rpctest

package sweep

import (
	"time"
)

var (
	// DefaultBatchWindowDuration specifies duration of the sweep batch
	// window. The sweep is held back during the batch window to allow more
	// inputs to be added and thereby lower the fee per input.
	//
	// To speed up integration tests waiting for a sweep to happen, the
	// batch window is shortened.
	DefaultBatchWindowDuration = 2 * time.Second
)
