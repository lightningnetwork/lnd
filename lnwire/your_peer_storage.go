package lnwire

import (
	"bytes"
	"io"
)

// YourPeerStorage is sent in response to a PeerStorage message. It contains
// the data in the last PeerStorage message received by the peer. It is also
// sent on reconnection to any peer we previously received PeerStorage from.
type YourPeerStorage struct {
	// Blob is the data backed up by a peer for another peer.
	Blob PeerStorageBlob
}

// NewYourPeerStorageMsg creates new instance of YourPeerStorage message object.
func NewYourPeerStorageMsg(data PeerStorageBlob) *YourPeerStorage {
	return &YourPeerStorage{
		Blob: data,
	}
}

// A compile time check to ensure YourPeerStorage implements the lnwire.Message
// interface.
var _ Message = (*YourPeerStorage)(nil)

// Decode deserializes a serialized YourPeerStorage message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *YourPeerStorage) Decode(r io.Reader, _ uint32) error {
	return ReadElement(r, &msg.Blob)
}

// Encode serializes the target YourPeerStorage into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *YourPeerStorage) Encode(w *bytes.Buffer, _ uint32) error {
	return WriteElement(w, msg.Blob)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *YourPeerStorage) MsgType() MessageType {
	return MsgYourPeerStorage
}
