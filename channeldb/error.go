package channeldb

import "fmt"

var (
	ErrNoExists         = fmt.Errorf("channel db has not yet been created")
	ErrNoActiveChannels = fmt.Errorf("no active channels exist")
)
