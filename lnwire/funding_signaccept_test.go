package lnwire

import (
	"testing"
)

var (
	//funding sign accept
	fundingSignAccept = &FundingSignAccept{
		ReservationID: uint64(12345678),
		CommitSig:     commitSig,
		FundingTXSigs: ptrFundingTXSigs,
	}
	fundingSignAcceptSerializedString  = "0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
	fundingSignAcceptSerializedMessage = "0709110b000000dc000000df0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
)

func TestFundingSignAcceptEncodeDecode(t *testing.T) {
	//All of these types being passed are of the message interface type
	//Test serialization, runs: message.Encode(b, 0)
	//Returns bytes
	//Compares the expected serialized string from the original
	s := SerializeTest(t, fundingSignAccept, fundingSignAcceptSerializedString, filename)

	//Test deserialization, runs: message.Decode(s, 0)
	//Makes sure the deserialized struct is the same as the original
	newMessage := NewFundingSignAccept()
	DeserializeTest(t, s, newMessage, fundingSignAccept)

	//Test message using Message interface
	//Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	//Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, fundingSignAccept, fundingSignAcceptSerializedMessage)
}
