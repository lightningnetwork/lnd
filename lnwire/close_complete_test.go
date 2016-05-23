package lnwire

import (
	"testing"
)

var (
	closeComplete = &CloseComplete{
		ChannelID:         uint64(12345678),
		ResponderCloseSig: commitSig,
	}
	closeCompleteSerializedString  = "0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1dfe3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	closeCompleteSerializedMessage = "0709110b000001360000006f0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1dfe3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestCloseCompleteEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, closeComplete, closeCompleteSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewCloseComplete()
	DeserializeTest(t, s, newMessage, closeComplete)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, closeComplete, closeCompleteSerializedMessage)
}
