// +build gofuzz

package lnwirefuzz

import (
	"bytes"
	"reflect"

	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_node_announcement is used by go-fuzz.
func Fuzz_node_announcement(data []byte) int {
	// Prefix with MsgNodeAnnouncement.
	data = prefixWithMsgType(data, lnwire.MsgNodeAnnouncement)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.NodeAnnouncement{}

	// We have to do this here instead of in fuzz.Harness so that
	// reflect.DeepEqual isn't called. Address (de)serialization messes up
	// the fuzzing assertions.

	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	// Make sure byte array length (excluding 2 bytes for message type) is
	// less than max payload size for the wire message. We check this because
	// otherwise `go-fuzz` will keep creating inputs that crash on ReadMessage
	// due to a large message size.
	payloadLen := uint32(len(data)) - 2
	if payloadLen > emptyMsg.MaxPayloadLength(0) {
		// Ignore this input - max payload constraint violated.
		return -1
	}

	msg, err := lnwire.ReadMessage(r, 0)
	if err != nil {
		// go-fuzz generated []byte that cannot be represented as a
		// wire message but we will return 0 so go-fuzz can modify the
		// input.
		return 0
	}

	// We will serialize the message into a new bytes buffer.
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, msg, 0); err != nil {
		// Could not serialize message into bytes buffer, panic
		panic(err)
	}

	// Deserialize the message from the serialized bytes buffer, and then
	// assert that the original message is equal to the newly deserialized
	// message.
	newMsg, err := lnwire.ReadMessage(&b, 0)
	if err != nil {
		// Could not deserialize message from bytes buffer, panic
		panic(err)
	}

	// Now compare every field instead of using reflect.DeepEqual for the
	// Addresses field.
	var shouldPanic bool
	first := msg.(*lnwire.NodeAnnouncement)
	second := newMsg.(*lnwire.NodeAnnouncement)
	if !bytes.Equal(first.Signature[:], second.Signature[:]) {
		shouldPanic = true
	}

	if !reflect.DeepEqual(first.Features, second.Features) {
		shouldPanic = true
	}

	if first.Timestamp != second.Timestamp {
		shouldPanic = true
	}

	if !bytes.Equal(first.NodeID[:], second.NodeID[:]) {
		shouldPanic = true
	}

	if !reflect.DeepEqual(first.RGBColor, second.RGBColor) {
		shouldPanic = true
	}

	if !bytes.Equal(first.Alias[:], second.Alias[:]) {
		shouldPanic = true
	}

	if len(first.Addresses) != len(second.Addresses) {
		shouldPanic = true
	}

	for i := range first.Addresses {
		if first.Addresses[i].String() != second.Addresses[i].String() {
			shouldPanic = true
			break
		}
	}

	if !reflect.DeepEqual(first.ExtraOpaqueData, second.ExtraOpaqueData) {
		shouldPanic = true
	}

	if shouldPanic {
		panic("original message and deserialized message are not equal")
	}

	// Add this input to the corpus.
	return 1
}
