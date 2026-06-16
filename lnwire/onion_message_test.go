package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestOnionMessageWireSizeMatchesEncode verifies that the value produced by
// OnionMessage.WireSize — the value fed to the onion message rate limiter on
// every incoming packet — matches the number of bytes WriteMessage actually
// emits for that same message. WireSize computes its result directly from the
// in-memory fields without round-tripping through Encode, which is fast but
// creates a risk of silent divergence if the OnionMessage wire format ever
// gains an optional TLV extension or extra field. This test is the
// compile-time-cheap regression guard that divergence does not go undetected.
func TestOnionMessageWireSizeMatchesEncode(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		msg, ok := (*OnionMessage)(nil).RandTestMessage(
			rt,
		).(*OnionMessage)
		require.True(
			rt, ok, "RandTestMessage did "+
				"not return an OnionMessage",
		)

		var buf bytes.Buffer
		written, err := WriteMessage(&buf, msg, 0)
		require.NoError(rt, err, "WriteMessage error")
		require.Equal(rt, written, msg.WireSize(),
			"WireSize=%d, WriteMessage wrote=%d bytes",
			msg.WireSize(), written,
		)
	})
}
