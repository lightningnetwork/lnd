package channeldb

import "fmt"

var (
	ErrNoExists = fmt.Errorf("channel db has not yet been created")
)
