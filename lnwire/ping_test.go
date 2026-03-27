package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPingDecodeNoReply tests that ping messages with num_pong_bytes >= 65532
// (the "no reply needed" range per BOLT #1) are correctly decoded without
// error. This is a regression test for issue #10671.
func TestPingDecodeNoReply(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		numPongBytes uint16
		padding      PingPayload
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
			padding:      []byte{},
		},
		{
			name:         "normal pong 0 bytes",
			numPongBytes: 0,
			padding:      []byte{0xaa, 0xbb},
		},
		{
			name:         "normal pong max allowed",
			numPongBytes: MaxPongBytes,
			padding:      []byte{},
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
			require.NoError(t, err, "failed to encode ping")

			// Decode it back.
			decodedPing := &Ping{}
			err = decodedPing.Decode(&buf, 0)
			require.NoErrorf(t, err, "failed to decode ping with "+
				"num_pong_bytes=%d", tc.numPongBytes)

			// Verify fields match.
			require.Equal(t, tc.numPongBytes, decodedPing.NumPongBytes)
			require.Equal(t, tc.padding, decodedPing.PaddingBytes)
		})
	}
}
