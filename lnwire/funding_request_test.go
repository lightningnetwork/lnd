package lnwire

import (
	"testing"

	"github.com/roasbeef/btcutil"
)

var (
	// Need to do this here
	_ = copy(revocationHash[:], revocationHashBytes)

	// funding request
	fundingRequest = &FundingRequest{
		ReservationID:          uint64(12345678),
		ChannelType:            uint8(0),
		RequesterFundingAmount: btcutil.Amount(100000000),
		RequesterReserveAmount: btcutil.Amount(131072),
		MinFeePerKb:            btcutil.Amount(20000),
		MinTotalFundingAmount:  btcutil.Amount(150000000),
		LockTime:               uint32(4320), // 30 block-days
		FeePayer:               uint8(0),
		PaymentAmount:          btcutil.Amount(1234567),
		MinDepth:               uint32(6),
		RevocationHash:         revocationHash,
		Pubkey:                 pubKey,
		DeliveryPkScript:       deliveryPkScript,
		ChangePkScript:         changePkScript,
		Inputs:                 inputs,
	}
	fundingRequestSerializedString  = "0000000000bc614e000000000005f5e1000000000008f0d1804132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e20000000000012d68700000006000010e0001976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
	fundingRequestSerializedMessage = "0709110b000000c8000000ec0000000000bc614e000000000005f5e1000000000008f0d1804132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e20000000000012d68700000006000010e0001976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
)

func TestFundingRequestEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, fundingRequest, fundingRequestSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewFundingRequest()
	DeserializeTest(t, s, newMessage, fundingRequest)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, fundingRequest, fundingRequestSerializedMessage)
}
