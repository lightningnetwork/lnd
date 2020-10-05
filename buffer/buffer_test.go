package buffer_test

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/buffer"
)

// TestRecycleSlice asserts that RecycleSlice always zeros a byte slice.
func TestRecycleSlice(t *testing.T) {
	tests := []struct {
		name  string
		slice []byte
	}{
		{
			name: "length zero",
		},
		{
			name:  "length one",
			slice: []byte("a"),
		},
		{
			name:  "length power of two length",
			slice: bytes.Repeat([]byte("b"), 16),
		},
		{
			name:  "length non power of two",
			slice: bytes.Repeat([]byte("c"), 27),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer.RecycleSlice(test.slice)

			expSlice := make([]byte, len(test.slice))
			if !bytes.Equal(expSlice, test.slice) {
				t.Fatalf("slice not recycled, want: %v, got: %v",
					expSlice, test.slice)
			}
		})
	}
}
