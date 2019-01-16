package wirefuzz

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz is used by go-fuzz to fuzz for potentially malicious input
func Fuzz(data []byte) int {
	// Because go-fuzz requires this function signature with a []byte parameter,
	// and we want to emulate the behavior of mainScenario in lnwire_test.go,
	// we first parse the []byte parameter into a Message type.

	// Parsing []byte into Message
	r := bytes.NewReader(data)
	msg, err := lnwire.ReadMessage(r, 0)
	if err != nil {
		// Ignore this input - go-fuzz generated []byte that cannot be represented as Message
		return 0
	}

	// We will serialize Message into a new bytes buffer
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, msg, 0); err != nil {
		// Could not serialize Message into bytes buffer, panic
		panic(err)
	}

	// Make sure serialized bytes buffer (excluding 2 bytes for message type
	// is less than max payload size for this specific message,.
	payloadLen := uint32(b.Len()) - 2
	if payloadLen > msg.MaxPayloadLength(0) {
		// Ignore this input - max payload constraint violated
		return 0
	}

	// Deserialize the message from the serialized bytes buffer and
	// assert that the original message is equal to the newly deserialized message.
	newMsg, err := lnwire.ReadMessage(&b, 0)
	if err != nil {
		// Could not deserialize message from bytes buffer, panic
		panic(err)
	}
	if !reflect.DeepEqual(msg, newMsg) {
		// Deserialized message and original message are not deeply equal
		panic(fmt.Errorf("Deserialized message and original message " +
			"are not deeply equal."))
	}

	return 1
}
