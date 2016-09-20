package sphinx

import "fmt"

var (
	ErrReplayedPacket = fmt.Errorf("sphinx packet replay attempted")
)
