package lnwire

import (
	"bytes"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestSerializedSize uses property-based testing to verify that
// SerializedSize returns the correct value for randomly generated messages.
func TestSerializedSize(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// Pick a random message type.
		msgType := rapid.Custom(func(t *rapid.T) MessageType {
			return MessageType(
				rapid.IntRange(
					0, int(math.MaxUint16),
				).Draw(t, "msgType"),
			)
		}).Draw(t, "msgType")

		// Create an empty message of the given type.
		m, err := MakeEmptyMessage(msgType)

		// An error means this isn't a valid message type, so we skip
		// it.
		if err != nil {
			return
		}

		testMsg, ok := m.(TestMessage)
		require.True(
			t, ok, "message type %s does not "+
				"implement TestMessage", msgType,
		)

		// Use the testMsg to make a new random message.
		msg := testMsg.RandTestMessage(t)

		// Type assertion to ensure the message implements
		// SizeableMessage.
		sizeMsg, ok := msg.(SizeableMessage)
		require.True(
			t, ok, "message type %s does not "+
				"implement SizeableMessage", msgType,
		)

		// Get the size using SerializedSize.
		size, err := sizeMsg.SerializedSize()
		require.NoError(t, err, "SerializedSize error")

		// Get the size by actually serializing the message.
		var buf bytes.Buffer
		writtenBytes, err := WriteMessage(&buf, msg, 0)
		require.NoError(t, err, "WriteMessage error")

		// The SerializedSize should match the number of bytes written.
		require.Equal(t, uint32(writtenBytes), size,
			"SerializedSize = %d, actual bytes "+
				"written = %d for message type %s (populated)",
			size, writtenBytes, msgType)
	})
}
