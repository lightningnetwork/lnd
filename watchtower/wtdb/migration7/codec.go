package migration7

import "encoding/hex"

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte

// String returns a hex encoding of the session id.
func (s SessionID) String() string {
	return hex.EncodeToString(s[:])
}

// ChannelID is a series of 32-bytes that uniquely identifies all channels
// within the network. The ChannelID is computed using the outpoint of the
// funding transaction (the txid, and output index). Given a funding output the
// ChannelID can be calculated by XOR'ing the big-endian serialization of the
// txid and the big-endian serialization of the output index, truncated to
// 2 bytes.
type ChannelID [32]byte

// String returns the string representation of the ChannelID. This is just the
// hex string encoding of the ChannelID itself.
func (c ChannelID) String() string {
	return hex.EncodeToString(c[:])
}
