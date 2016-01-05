package lnwire

import (
	"testing"
)

var (
	htlcTimeoutAccept = &HTLCTimeoutAccept{
		ChannelID: uint64(12345678),
		StagingID: uint64(12345),
	}
	htlcTimeoutAcceptSerializedString  = "0000000000bc614e0000000000003039"
	htlcTimeoutAcceptSerializedMessage = "0709110b0000051e000000100000000000bc614e0000000000003039"
)

func TestHTLCTimeoutAcceptEncodeDecode(t *testing.T) {
	//All of these types being passed are of the message interface type
	//Test serialization, runs: message.Encode(b, 0)
	//Returns bytes
	//Compares the expected serialized string from the original
	s := SerializeTest(t, htlcTimeoutAccept, htlcTimeoutAcceptSerializedString, filename)

	//Test deserialization, runs: message.Decode(s, 0)
	//Makes sure the deserialized struct is the same as the original
	newMessage := NewHTLCTimeoutAccept()
	DeserializeTest(t, s, newMessage, htlcTimeoutAccept)

	//Test message using Message interface
	//Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	//Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, htlcTimeoutAccept, htlcTimeoutAcceptSerializedMessage)
}
