package lnwire

import (
	"bytes"
	"math"
	"testing"
)

// TestMaxPongBytes tests that we'll fail to decode a Ping message that wants
// more pong bytes that we can actually encode while adhering to the max
// message limit.
func TestMaxPongBytes(t *testing.T) {
	t.Parallel()

	msg := Ping{
		NumPongBytes: math.MaxUint16,
	}

	var b bytes.Buffer
	if _, err := WriteMessage(&b, &msg, 0); err != nil {
		t.Fatalf("unable to write msg: %v", err)
	}

	_, err := ReadMessage(&b, 0)
	if err != ErrMaxPongBytesExceeded {
		t.Fatalf("incorrect error: %v, expected "+
			"ErrMaxPongBytesExceeded", err)
	}
}
