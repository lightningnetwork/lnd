package lnwire

import (
	"github.com/btcsuite/btcutil"
	"testing"
)

var (
	// Need to to do this here
	_ = copy(revocationHash[:], revocationHashBytes)

	commitSignature = &CommitSignature{
		ChannelID:             uint64(12345678),
		CommitmentHeight:      uint64(12345),
		LastCommittedKeyAlice: HTLCKey(12345),
		LastCommittedKeyBob:   HTLCKey(54321),
		RevocationHash:        revocationHash,
		Fee:                   btcutil.Amount(10000),
		CommitSig:             commitSig,
	}
	commitSignatureSerializedString  = "0000000000bc614e00000000000030390000000000003039000000000000d4314132b6b48371f7b022a16eacb9b2b0ebee134d4100000000000027104630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df"
	commitSignatureSerializedMessage = "0709110b000007d0000000830000000000bc614e00000000000030390000000000003039000000000000d4314132b6b48371f7b022a16eacb9b2b0ebee134d4100000000000027104630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df"
)

func TestCommitSignatureEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, commitSignature, commitSignatureSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewCommitSignature()
	DeserializeTest(t, s, newMessage, commitSignature)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, commitSignature, commitSignatureSerializedMessage)
}
