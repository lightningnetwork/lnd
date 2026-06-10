package lnwire

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPingDecodeAllowsNoReplyPongSizes asserts that ping messages using the
// BOLT 1 no-reply sentinel range still deserialize successfully.
func TestPingDecodeAllowsNoReplyPongSizes(t *testing.T) {
	t.Parallel()

	// Arrange: Pick values from the BOLT 1 no-reply range. These
	// pings are valid on the wire and should decode successfully
	// even though they do not require a pong response.
	testCases := []uint16{65532, 65535}

	for _, numPongBytes := range testCases {
		testName := strconv.FormatUint(uint64(numPongBytes), 10)

		t.Run(testName, func(t *testing.T) {
			// Arrange: Encode a ping carrying a no-reply pong
			// size together with a small payload so we exercise
			// the normal wire format.
			var buf bytes.Buffer

			want := &Ping{
				NumPongBytes: numPongBytes,
				PaddingBytes: PingPayload{1, 2, 3},
			}

			_, err := WriteMessage(&buf, want, 0)
			require.NoError(t, err)

			// Act: Decode the serialized message through the
			// standard parser.
			msg, err := ReadMessage(bytes.NewReader(buf.Bytes()), 0)
			require.NoError(t, err)

			// Assert: The decoded ping matches the original
			// input exactly.
			got, ok := msg.(*Ping)
			require.True(t, ok)
			require.Equal(t, want, got)
		})
	}
}
