package lnwire

import (
	"bytes"
	"io"
)

// PeerStorageRetrieval stores the last PeerStorage message received from
// that particular peer. It is sent to that peer on reconnection after the
// `init` message and before the `channelReestablish` message on reconnection.
type PeerStorageRetrieval struct {
	// Blob contains data a peer backs up for another.
	Blob PeerStorageBlob
}

// NewPeerStorageRetrievalMsg creates a new instance of PeerStorageRetrieval
// message object.
func NewPeerStorageRetrievalMsg(data PeerStorageBlob) *PeerStorageRetrieval {
	return &PeerStorageRetrieval{
		Blob: data,
	}
}

// A compile time check to ensure PeerStorageRetrieval implements the
// lnwire.Message interface.
var _ Message = (*PeerStorageRetrieval)(nil)

// Decode deserializes a serialized PeerStorageRetrieval message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorageRetrieval) Decode(r io.Reader, _ uint32) error {
	return ReadElement(r, &msg.Blob)
}

// Encode serializes the target PeerStorageRetrieval into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorageRetrieval) Encode(w *bytes.Buffer, _ uint32) error {
	return WriteElement(w, msg.Blob)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorageRetrieval) MsgType() MessageType {
	return MsgPeerStorageRetrieval
}
