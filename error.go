package sphinx

import "fmt"

var (
	// ErrReplayedPacket is an error returned when a packet is rejected
	// during processing due to being an attempted replay or probing
	// attempt.
	ErrReplayedPacket = fmt.Errorf("sphinx packet replay attempted")
)
