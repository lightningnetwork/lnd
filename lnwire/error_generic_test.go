package lnwire

import (
	"testing"
)

var (
	errorGeneric = &ErrorGeneric{
		ChannelID: uint64(12345678),
		Problem:   "Hello world!",
	}
	errorGenericSerializedString  = "0000000000bc614e000c48656c6c6f20776f726c6421"
	errorGenericSerializedMessage = "0709110b00000fa0000000160000000000bc614e000c48656c6c6f20776f726c6421"
)

func TestErrorGenericEncodeDecode(t *testing.T) {
	//All of these types being passed are of the message interface type
	//Test serialization, runs: message.Encode(b, 0)
	//Returns bytes
	//Compares the expected serialized string from the original
	s := SerializeTest(t, errorGeneric, errorGenericSerializedString, filename)

	//Test deserialization, runs: message.Decode(s, 0)
	//Makes sure the deserialized struct is the same as the original
	newMessage := NewErrorGeneric()
	DeserializeTest(t, s, newMessage, errorGeneric)

	//Test message using Message interface
	//Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	//Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, errorGeneric, errorGenericSerializedMessage)
}
