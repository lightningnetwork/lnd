//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"

	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// prefixWithMsgType takes []byte and adds a wire protocol prefix
// to make the []byte into an actual message to be used in fuzzing.
func prefixWithMsgType(data []byte, prefix wtwire.MessageType) []byte {
	var prefixBytes [2]byte
	binary.BigEndian.PutUint16(prefixBytes[:], uint16(prefix))
	data = append(prefixBytes[:], data...)
	return data
}

// harness performs the actual fuzz testing of the appropriate wire message.
// This function will check that the passed-in message passes wire length checks,
// is a valid message once deserialized, and passes a sequence of serialization
// and deserialization checks. Returns an int that determines whether the input
// is unique or not.
func harness(data []byte, emptyMsg wtwire.Message) int {
	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	// Make sure byte array length (excluding 2 bytes for message type) is
	// less than max payload size for the wire message. We check this because
	// otherwise `go-fuzz` will keep creating inputs that crash on ReadMessage
	// due to a large message size.
	payloadLen := uint32(len(data)) - 2
	if payloadLen > emptyMsg.MaxPayloadLength(0) {
		// Ignore this input - max payload constraint violated.
		return 1
	}

	msg, err := wtwire.ReadMessage(r, 0)
	if err != nil {
		// go-fuzz generated []byte that cannot be represented as a
		// wire message but we will return 0 so go-fuzz can modify the
		// input.
		return 1
	}

	// We will serialize the message into a new bytes buffer.
	var b bytes.Buffer
	if _, err := wtwire.WriteMessage(&b, msg, 0); err != nil {
		// Could not serialize message into bytes buffer, panic.
		panic(err)
	}

	// Deserialize the message from the serialized bytes buffer, and then
	// assert that the original message is equal to the newly deserialized
	// message.
	newMsg, err := wtwire.ReadMessage(&b, 0)
	if err != nil {
		// Could not deserialize message from bytes buffer, panic.
		panic(err)
	}

	if !reflect.DeepEqual(msg, newMsg) {
		// Deserialized message and original message are not
		// deeply equal.
		panic(fmt.Errorf("deserialized message and original message " +
			"are not deeply equal."))
	}

	// Add this input to the corpus.
	return 1
}
