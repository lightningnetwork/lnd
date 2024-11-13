package wtwire

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// prefixWithMsgType takes []byte and adds a wire protocol prefix
// to make the []byte into an actual message to be used in fuzzing.
func prefixWithMsgType(data []byte, prefix MessageType) []byte {
	var prefixBytes [2]byte
	binary.BigEndian.PutUint16(prefixBytes[:], uint16(prefix))
	data = append(prefixBytes[:], data...)

	return data
}

// wireMsgHarness performs the actual fuzz testing of the appropriate wire
// message. This function will check that the passed-in message passes wire
// length checks, is a valid message once deserialized, and passes a sequence of
// serialization and deserialization checks. emptyMsg must be an empty Message
// of the type to be fuzzed, as it is used to determine the appropriate prefix
// bytes and max payload length for decoding.
func wireMsgHarness(t *testing.T, data []byte, emptyMsg Message) {
	t.Helper()

	// Make sure byte array length is less than max payload size for the
	// wire message.
	payloadLen := uint32(len(data))
	if payloadLen > emptyMsg.MaxPayloadLength(0) {
		// Ignore this input - max payload constraint violated.
		return
	}

	data = prefixWithMsgType(data, emptyMsg.MsgType())

	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	msg, err := ReadMessage(r, 0)
	if err != nil {
		return
	}

	// We will serialize the message into a new bytes buffer.
	var b bytes.Buffer
	_, err = WriteMessage(&b, msg, 0)
	require.NoError(t, err)

	// Deserialize the message from the serialized bytes buffer, and then
	// assert that the original message is equal to the newly deserialized
	// message.
	newMsg, err := ReadMessage(&b, 0)
	require.NoError(t, err)
	require.Equal(t, msg, newMsg)
}

func FuzzCreateSessionReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &CreateSessionReply{})
	})
}

func FuzzCreateSession(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &CreateSession{})
	})
}

func FuzzDeleteSessionReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &DeleteSessionReply{})
	})
}

func FuzzDeleteSession(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &DeleteSession{})
	})
}

func FuzzError(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &Error{})
	})
}

func FuzzInit(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &Init{})
	})
}

func FuzzStateUpdateReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &StateUpdateReply{})
	})
}

func FuzzStateUpdate(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, &StateUpdate{})
	})
}
