package lnwire

import (
	"testing"
)

var (
	htlcAddAccept = &HTLCAddAccept{
		ChannelID: uint64(12345678),
		HTLCKey: HTLCKey(12345),
	}
	htlcAddAcceptSerializedString  = "0000000000bc614e0000000000003039"
	htlcAddAcceptSerializedMessage = "0709110b000003f2000000100000000000bc614e0000000000003039"
)

func TestHTLCAddAcceptEncodeDecode(t *testing.T) {
	//All of these types being passed are of the message interface type
	//Test serialization, runs: message.Encode(b, 0)
	//Returns bytes
	//Compares the expected serialized string from the original
	s := SerializeTest(t, htlcAddAccept, htlcAddAcceptSerializedString, filename)

	//Test deserialization, runs: message.Decode(s, 0)
	//Makes sure the deserialized struct is the same as the original
	newMessage := NewHTLCAddAccept()
	DeserializeTest(t, s, newMessage, htlcAddAccept)

	//Test message using Message interface
	//Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	//Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, htlcAddAccept, htlcAddAcceptSerializedMessage)
}
