package lnwire

import (
	"bytes"
	"errors"
	"io"
)

// MaxPeerStorageBytes is the maximum size in bytes of the blob in peer storage
// message.
const MaxPeerStorageBytes = 65531

// ErrPeerStorageBytesExceeded is returned when the Peer Storage blob's size
// exceeds MaxPeerStorageBytes.
var ErrPeerStorageBytesExceeded = errors.New("peer storage bytes exceeded")

// PeerStorage contains a data blob that the sending peer would like the
// receiving peer to store.
type PeerStorage struct {
	// Blob is data for the receiving peer to store from the sender.
	Blob PeerStorageBlob
}

// NewPeerStorageMsg creates new instance of PeerStorage message object.
func NewPeerStorageMsg(data PeerStorageBlob) (*PeerStorage, error) {
	if len(data) > MaxPeerStorageBytes {
		return nil, ErrPeerStorageBytesExceeded
	}

	return &PeerStorage{
		Blob: data,
	}, nil
}

// A compile time check to ensure PeerStorage implements the lnwire.Message
// interface.
var _ Message = (*PeerStorage)(nil)

// Decode deserializes a serialized PeerStorage message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorage) Decode(r io.Reader, _ uint32) error {
	return ReadElement(r, &msg.Blob)
}

// Encode serializes the target PeerStorage into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorage) Encode(w *bytes.Buffer, _ uint32) error {
	return WriteElement(w, msg.Blob)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *PeerStorage) MsgType() MessageType {
	return MsgPeerStorage
}
