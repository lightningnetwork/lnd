package lnwire

import (
	"bytes"
	"fmt"
	"io"
)

const (
	// MaxPeerStorageBlob is the maximum size of a peer storage blob in
	// bytes. This is 65531 bytes per the BOLT specification, which is
	// the maximum message payload (65535) minus the 2-byte type and
	// 2-byte length prefix.
	MaxPeerStorageBlob = 65531
)

// PeerStoragePayload is a set of opaque bytes that represent an encrypted
// backup blob stored by a peer on our behalf, or by us on behalf of a peer.
type PeerStoragePayload []byte

// PeerStorage is the message (type 7) that a node sends to a connected peer
// to request storage of an encrypted backup blob. The blob is opaque to the
// storing peer.
type PeerStorage struct {
	// Blob is the encrypted backup data to be stored.
	Blob PeerStoragePayload
}

// A compile time check to ensure PeerStorage implements the lnwire.Message
// interface.
var _ Message = (*PeerStorage)(nil)

// A compile time check to ensure PeerStorage implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*PeerStorage)(nil)

// Decode deserializes a serialized PeerStorage message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorage) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r, &p.Blob)
}

// Encode serializes the target PeerStorage into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorage) Encode(w *bytes.Buffer, pver uint32) error {
	if len(p.Blob) > MaxPeerStorageBlob {
		return fmt.Errorf("peer storage blob of size %d exceeds max "+
			"size of %d", len(p.Blob), MaxPeerStorageBlob)
	}

	return WritePeerStoragePayload(w, p.Blob)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorage) MsgType() MessageType {
	return MsgPeerStorage
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (p *PeerStorage) SerializedSize() (uint32, error) {
	return MessageSerializedSize(p)
}

// PeerStorageRetrieval is the message (type 9) that a node sends to a
// connected peer upon reconnection to return the most recently stored
// encrypted backup blob.
type PeerStorageRetrieval struct {
	// Blob is the encrypted backup data being returned.
	Blob PeerStoragePayload
}

// A compile time check to ensure PeerStorageRetrieval implements the
// lnwire.Message interface.
var _ Message = (*PeerStorageRetrieval)(nil)

// A compile time check to ensure PeerStorageRetrieval implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*PeerStorageRetrieval)(nil)

// Decode deserializes a serialized PeerStorageRetrieval message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorageRetrieval) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r, &p.Blob)
}

// Encode serializes the target PeerStorageRetrieval into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorageRetrieval) Encode(w *bytes.Buffer, pver uint32) error {
	return WritePeerStoragePayload(w, p.Blob)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (p *PeerStorageRetrieval) MsgType() MessageType {
	return MsgPeerStorageRetrieval
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (p *PeerStorageRetrieval) SerializedSize() (uint32, error) {
	return MessageSerializedSize(p)
}
