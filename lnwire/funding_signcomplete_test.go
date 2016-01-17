package lnwire

import (
	"testing"
)

var (
	// funding response
	fundingSignComplete = &FundingSignComplete{
		ReservationID: uint64(12345678),
		TxID:          txid,
		FundingTXSigs: ptrFundingTXSigs,
	}
	fundingSignCompleteSerializedString  = "0000000000bc614efd95c6e5c9d5bcf9cfc7231b6a438e46c518c724d0b04b75cc8fddf84a254e3a02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
	fundingSignCompleteSerializedMessage = "0709110b000000e6000000b80000000000bc614efd95c6e5c9d5bcf9cfc7231b6a438e46c518c724d0b04b75cc8fddf84a254e3a02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
)

func TestFundingSignCompleteEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, fundingSignComplete, fundingSignCompleteSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewFundingSignComplete()
	DeserializeTest(t, s, newMessage, fundingSignComplete)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, fundingSignComplete, fundingSignCompleteSerializedMessage)
}
