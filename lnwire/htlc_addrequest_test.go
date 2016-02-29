package lnwire

import (
	"testing"
)

var (
	// Need to to do this here
	_                     = copy(revocationHash[:], revocationHashBytes)
	_                     = copy(redemptionHash[:], redemptionHashBytes)
	emptyRedemptionHashes = []*[20]byte{}
	redemptionHashes      = append(emptyRedemptionHashes, &redemptionHash)

	htlcAddRequest = &HTLCAddRequest{
		ChannelID:        uint64(12345678),
		HTLCKey:          HTLCKey(12345),
		Expiry:           uint32(144),
		Amount:           CreditsAmount(123456000),
		ContractType:     uint8(17),
		RedemptionHashes: redemptionHashes,

		Blob: []byte{255, 0, 255, 0, 255, 0, 255, 0},
	}
	htlcAddRequestSerializedString = "0000000000bc614e00000000000030390000009000000000075bca001100015b315ebabb0d8c0d94281caa2dfee69a1a00436e0008ff00ff00ff00ff00"

	htlcAddRequestSerializedMessage = "0709110b000003e80000003d0000000000bc614e00000000000030390000009000000000075bca001100015b315ebabb0d8c0d94281caa2dfee69a1a00436e0008ff00ff00ff00ff00"
)

func TestHTLCAddRequestEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, htlcAddRequest, htlcAddRequestSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewHTLCAddRequest()
	DeserializeTest(t, s, newMessage, htlcAddRequest)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, htlcAddRequest, htlcAddRequestSerializedMessage)
}
