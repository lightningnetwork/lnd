package fn

import (
	"errors"
	"fmt"
	"time"
)

// ErrChannelClosed is returned when a receive channel is closed before a
// value is received.
var ErrChannelClosed = errors.New("channel closed")

// RecvOrTimeout attempts to recv over chan c, returning the value. If the
// timeout passes before the recv succeeds, an error is returned
func RecvOrTimeout[T any](c <-chan T, timeout time.Duration) (T, error) {
	select {
	case m, ok := <-c:
		if !ok {
			var zero T
			return zero, ErrChannelClosed
		}

		return m, nil

	case <-time.After(timeout):
		var zero T
		return zero, fmt.Errorf("timeout hit")
	}
}

// RecvResp takes three channels: a response channel, an error channel and a
// quit channel. If either of these channels are sent on, then the function
// will exit with that response. This can be used to wait for a response,
// error, or a quit signal.
func RecvResp[T any](r <-chan T, e <-chan error, q <-chan struct{}) (T, error) {
	var noResp T

	select {
	case resp, ok := <-r:
		if !ok {
			return noResp, ErrChannelClosed
		}

		return resp, nil

	case err, ok := <-e:
		if !ok {
			return noResp, ErrChannelClosed
		}

		return noResp, err

	case <-q:
		return noResp, fmt.Errorf("quitting")
	}
}
