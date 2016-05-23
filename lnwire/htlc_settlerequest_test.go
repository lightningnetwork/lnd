package lnwire

import (
	"testing"
)

var (
	// Need to to do this here
	_                     = copy(redemptionHash[:], redemptionHashBytes)
	emptyRedemptionProofs = [][20]byte{}
	redemptionProofs      = append(emptyRedemptionProofs, redemptionHash)

	htlcSettleRequest = &HTLCSettleRequest{
		ChannelID:        uint64(12345678),
		HTLCKey:          HTLCKey(12345),
		RedemptionProofs: redemptionProofs,
	}
	htlcSettleRequestSerializedString  = "0000000000bc614e000000000000303900015b315ebabb0d8c0d94281caa2dfee69a1a00436e"
	htlcSettleRequestSerializedMessage = "0709110b0000044c000000260000000000bc614e000000000000303900015b315ebabb0d8c0d94281caa2dfee69a1a00436e"
)

func TestHTLCSettleRequestEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, htlcSettleRequest, htlcSettleRequestSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewHTLCSettleRequest()
	DeserializeTest(t, s, newMessage, htlcSettleRequest)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, htlcSettleRequest, htlcSettleRequestSerializedMessage)
}
