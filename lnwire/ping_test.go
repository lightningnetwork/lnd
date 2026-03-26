package lnwire

import (
	"bytes"
	"testing"
)

// TestPingDecodeNoReply tests that ping messages with num_pong_bytes >= 65532
// (the "no reply needed" range per BOLT #1) are correctly decoded without
// error. This is a regression test for issue #10671.
func TestPingDecodeNoReply(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		numPongBytes uint16
		padding      []byte
	}{
		{
			name:         "no reply needed 65532",
			numPongBytes: 65532,
			padding:      []byte{0x01, 0x02, 0x03},
		},
		{
			name:         "no reply needed 65533",
			numPongBytes: 65533,
			padding:      []byte{},
		},
		{
			name:         "no reply needed 65534",
			numPongBytes: 65534,
			padding:      []byte{0xff},
		},
		{
			name:         "no reply needed 65535 (max uint16)",
			numPongBytes: 65535,
			padding:      nil,
		},
		{
			name:         "normal pong 0 bytes",
			numPongBytes: 0,
			padding:      []byte{0xaa, 0xbb},
		},
		{
			name:         "normal pong max allowed",
			numPongBytes: MaxPongBytes,
			padding:      nil,
		},
		{
			name:         "normal pong boundary 65531",
			numPongBytes: 65531,
			padding:      []byte{0xde, 0xad},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a ping and encode it.
			ping := &Ping{
				NumPongBytes: tc.numPongBytes,
				PaddingBytes: tc.padding,
			}

			var buf bytes.Buffer
			err := ping.Encode(&buf, 0)
			if err != nil {
				t.Fatalf("failed to encode ping: %v", err)
			}

			// Decode it back.
			decodedPing := &Ping{}
			err = decodedPing.Decode(&buf, 0)
			if err != nil {
				t.Fatalf("failed to decode ping with "+
					"num_pong_bytes=%d: %v",
					tc.numPongBytes, err)
			}

			// Verify fields match.
			if decodedPing.NumPongBytes != tc.numPongBytes {
				t.Errorf("NumPongBytes mismatch: got %d, "+
					"want %d", decodedPing.NumPongBytes,
					tc.numPongBytes)
			}
			if !bytes.Equal(decodedPing.PaddingBytes, tc.padding) {
				t.Errorf("PaddingBytes mismatch: got %x, "+
					"want %x", decodedPing.PaddingBytes,
					tc.padding)
			}
		})
	}
}
