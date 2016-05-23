package lnwire

import (
	"testing"

	"github.com/roasbeef/btcutil"
)

var (
	// Need to do this here
	_ = copy(revocationHash[:], revocationHashBytes)

	// funding response
	fundingResponse = &FundingResponse{
		ChannelType:            uint8(1),
		ReservationID:          uint64(12345678),
		ResponderFundingAmount: btcutil.Amount(100000000),
		ResponderReserveAmount: btcutil.Amount(131072),
		MinFeePerKb:            btcutil.Amount(20000),
		MinDepth:               uint32(6),
		LockTime:               uint32(4320), // 30 block-days
		FeePayer:               uint8(1),
		RevocationHash:         revocationHash,
		Pubkey:                 pubKey,
		CommitSig:              commitSig,
		DeliveryPkScript:       deliveryPkScript,
		ChangePkScript:         changePkScript,
		Inputs:                 inputs,
	}
	fundingResponseSerializedString  = "0000000000bc614e010000000005f5e1004132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e2000000006000010e0011976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
	fundingResponseSerializedMessage = "0709110b000000d2000001230000000000bc614e010000000005f5e1004132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e2000000006000010e0011976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
)

func TestFundingResponseEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, fundingResponse, fundingResponseSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewFundingResponse()
	DeserializeTest(t, s, newMessage, fundingResponse)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, fundingResponse, fundingResponseSerializedMessage)
}
