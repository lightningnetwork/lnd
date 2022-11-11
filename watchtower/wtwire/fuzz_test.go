package wtwire

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// prefixWithMsgType takes []byte and adds a wire protocol prefix
// to make the []byte into an actual message to be used in fuzzing.
func prefixWithMsgType(data []byte, prefix MessageType) []byte {
	var prefixBytes [2]byte
	binary.BigEndian.PutUint16(prefixBytes[:], uint16(prefix))
	data = append(prefixBytes[:], data...)

	return data
}

// harness performs the actual fuzz testing of the appropriate wire message.
// This function will check that the passed-in message passes wire length
// checks, is a valid message once deserialized, and passes a sequence of
// serialization and deserialization checks. Returns an int that determines
// whether the input is unique or not.
func harness(t *testing.T, data []byte, emptyMsg Message) {
	t.Helper()

	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	// Make sure byte array length (excluding 2 bytes for message type) is
	// less than max payload size for the wire message.
	payloadLen := uint32(len(data)) - 2
	if payloadLen > emptyMsg.MaxPayloadLength(0) {
		// Ignore this input - max payload constraint violated.
		return
	}

	msg, err := ReadMessage(r, 0)
	if err != nil {
		return
	}

	// We will serialize the message into a new bytes buffer.
	var b bytes.Buffer
	if _, err := WriteMessage(&b, msg, 0); err != nil {
		// Could not serialize message into bytes buffer, panic.
		t.Fatal(err)
	}

	// Deserialize the message from the serialized bytes buffer, and then
	// assert that the original message is equal to the newly deserialized
	// message.
	newMsg, err := ReadMessage(&b, 0)
	if err != nil {
		// Could not deserialize message from bytes buffer, panic.
		t.Fatal(err)
	}

	if !reflect.DeepEqual(msg, newMsg) {
		// Deserialized message and original message are not
		// deeply equal.
		t.Fatal("deserialized message and original message " +
			"are not deeply equal.")
	}
}

func FuzzCreateSessionReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgCreateSessionReply.
		data = prefixWithMsgType(data, MsgCreateSessionReply)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := CreateSessionReply{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzCreateSession(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgCreateSession.
		data = prefixWithMsgType(data, MsgCreateSession)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := CreateSession{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzDeleteSessionReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgDeleteSessionReply.
		data = prefixWithMsgType(data, MsgDeleteSessionReply)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := DeleteSessionReply{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzDeleteSession(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgDeleteSession.
		data = prefixWithMsgType(data, MsgDeleteSession)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := DeleteSession{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzError(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgError.
		data = prefixWithMsgType(data, MsgError)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := Error{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzInit(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgInit.
		data = prefixWithMsgType(data, MsgInit)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := Init{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzStateUpdateReply(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgStateUpdateReply.
		data = prefixWithMsgType(data, MsgStateUpdateReply)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := StateUpdateReply{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}

func FuzzStateUpdate(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgStateUpdate.
		data = prefixWithMsgType(data, MsgStateUpdate)

		// Create an empty message so that the FuzzHarness func can
		// check if the max payload constraint is violated.
		emptyMsg := StateUpdate{}

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data, &emptyMsg)
	})
}
