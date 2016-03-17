package lnwire

import (
	"testing"
)

var (
	// Need to to do this here
	_ = copy(revocationHash[:], revocationHashBytes)
	_ = copy(nextHop[:], nextHopBytes)

	commitRevocation = &CommitRevocation{
		ChannelID:          uint64(12345678),
		RevocationProof:    revocationHash, // technically it's not a hash... fix later
		NextRevocationHash: nextHop,        // technically it's not a hash... fix later
	}
	commitRevocationSerializedString  = "0000000000bc614e4132b6b48371f7b022a16eacb9b2b0ebee134d4194a9ded5a30fc5944cb1e2cbcd980f30616a1440"
	commitRevocationSerializedMessage = "0709110b000007da000000300000000000bc614e4132b6b48371f7b022a16eacb9b2b0ebee134d4194a9ded5a30fc5944cb1e2cbcd980f30616a1440"
)

func TestCommitRevocationEncodeDecode(t *testing.T) {
	// All of these types being passed are of the message interface type
	// Test serialization, runs: message.Encode(b, 0)
	// Returns bytes
	// Compares the expected serialized string from the original
	s := SerializeTest(t, commitRevocation, commitRevocationSerializedString, filename)

	// Test deserialization, runs: message.Decode(s, 0)
	// Makes sure the deserialized struct is the same as the original
	newMessage := NewCommitRevocation()
	DeserializeTest(t, s, newMessage, commitRevocation)

	// Test message using Message interface
	// Serializes into buf: WriteMessage(buf, message, uint32(1), wire.TestNet3)
	// Deserializes into msg: _, msg, _ , err := ReadMessage(buf, uint32(1), wire.TestNet3)
	MessageSerializeDeserializeTest(t, commitRevocation, commitRevocationSerializedMessage)
}
