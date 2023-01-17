package migration4

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

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

// BackupID identifies a particular revoked, remote commitment by channel id and
// commitment height.
type BackupID struct {
	// ChanID is the channel id of the revoked commitment.
	ChanID ChannelID

	// CommitHeight is the commitment height of the revoked commitment.
	CommitHeight uint64
}

// Encode writes the BackupID from the passed io.Writer.
func (b *BackupID) Encode(w io.Writer) error {
	return WriteElements(w,
		b.ChanID,
		b.CommitHeight,
	)
}

// Decode reads a BackupID from the passed io.Reader.
func (b *BackupID) Decode(r io.Reader) error {
	return ReadElements(r,
		&b.ChanID,
		&b.CommitHeight,
	)
}

// String returns a human-readable encoding of a BackupID.
func (b BackupID) String() string {
	return fmt.Sprintf("backup(%v, %d)", b.ChanID, b.CommitHeight)
}

// WriteElements serializes a variadic list of elements into the given
// io.Writer.
func WriteElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		if err := WriteElement(w, element); err != nil {
			return err
		}
	}

	return nil
}

// ReadElements deserializes the provided io.Reader into a variadic list of
// target elements.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		if err := ReadElement(r, element); err != nil {
			return err
		}
	}

	return nil
}

// WriteElement serializes a single element into the provided io.Writer.
func WriteElement(w io.Writer, element interface{}) error {
	switch e := element.(type) {
	case ChannelID:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case uint64:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unexpected type")
	}

	return nil
}

// ReadElement deserializes a single element from the provided io.Reader.
func ReadElement(r io.Reader, element interface{}) error {
	switch e := element.(type) {
	case *ChannelID:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *uint64:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unexpected type")
	}

	return nil
}
