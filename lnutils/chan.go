package lnutils

import (
	"fmt"
	"time"
)

// RecvOrTimeout attempts to recv over chan c, returning the value. If the
// timeout passes before the recv succeeds, an error is returned.
func RecvOrTimeout[T any](c <-chan T, timeout time.Duration) (*T, error) {
	select {
	case m := <-c:
		return &m, nil

	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout hit")
	}
}
