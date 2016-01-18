package lnwire

import (
	"testing"
)

var (
	htlcTimeoutRequest = &HTLCTimeoutRequest{
		ChannelID: uint64(12345678),
		HTLCKey:   HTLCKey(12345),
	}
	htlcTimeoutRequestSerializedString  = "0000000000bc614e0000000000003039"
	htlcTimeoutRequestSerializedMessage = "0709110b00000514000000100000000000bc614e0000000000003039"
)

func TestHTLCTimeoutRequestEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, htlcTimeoutRequest, htlcTimeoutRequestSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewHTLCTimeoutRequest()
	DeserializeTest(t, s, newMessage, htlcTimeoutRequest)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, htlcTimeoutRequest, htlcTimeoutRequestSerializedMessage)
}
