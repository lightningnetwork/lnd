package migration8

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// BreachHintSize is the length of the identifier used to detect remote
// commitment broadcasts.
const BreachHintSize = 16

// BreachHint is the first 16-bytes of SHA256(txid), which is used to identify
// the breach transaction.
type BreachHint [BreachHintSize]byte

// ChannelID is a series of 32-bytes that uniquely identifies all channels
// within the network. The ChannelID is computed using the outpoint of the
// funding transaction (the txid, and output index). Given a funding output the
// ChannelID can be calculated by XOR'ing the big-endian serialization of the
// txid and the big-endian serialization of the output index, truncated to
// 2 bytes.
type ChannelID [32]byte

// writeBigSize will encode the given uint64 as a BigSize byte slice.
func writeBigSize(i uint64) ([]byte, error) {
	var b bytes.Buffer
	err := tlv.WriteVarInt(&b, i, &[8]byte{})
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// readBigSize converts the given byte slice into a uint64 and assumes that the
// bytes slice is using BigSize encoding.
func readBigSize(b []byte) (uint64, error) {
	r := bytes.NewReader(b)
	i, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return 0, err
	}

	return i, nil
}

// CommittedUpdate holds a state update sent by a client along with its
// allocated sequence number and the exact remote commitment the encrypted
// justice transaction can rectify.
type CommittedUpdate struct {
	// SeqNum is the unique sequence number allocated by the session to this
	// update.
	SeqNum uint16

	CommittedUpdateBody
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

	case BreachHint:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case []byte:
		if err := wire.WriteVarBytes(w, 0, e); err != nil {
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

	case *BreachHint:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *[]byte:
		bytes, err := wire.ReadVarBytes(r, 0, 66000, "[]byte")
		if err != nil {
			return err
		}

		*e = bytes

	default:
		return fmt.Errorf("unexpected type")
	}

	return nil
}

// CommittedUpdateBody represents the primary components of a CommittedUpdate.
// On disk, this is stored under the sequence number, which acts as its key.
type CommittedUpdateBody struct {
	// BackupID identifies the breached commitment that the encrypted blob
	// can spend from.
	BackupID BackupID

	// Hint is the 16-byte prefix of the revoked commitment transaction ID.
	Hint BreachHint

	// EncryptedBlob is a ciphertext containing the sweep information for
	// exacting justice if the commitment transaction matching the breach
	// hint is broadcast.
	EncryptedBlob []byte
}

// Encode writes the CommittedUpdateBody to the passed io.Writer.
func (u *CommittedUpdateBody) Encode(w io.Writer) error {
	err := u.BackupID.Encode(w)
	if err != nil {
		return err
	}

	return WriteElements(w,
		u.Hint,
		u.EncryptedBlob,
	)
}

// Decode reads a CommittedUpdateBody from the passed io.Reader.
func (u *CommittedUpdateBody) Decode(r io.Reader) error {
	err := u.BackupID.Decode(r)
	if err != nil {
		return err
	}

	return ReadElements(r,
		&u.Hint,
		&u.EncryptedBlob,
	)
}

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte

// String returns a hex encoding of the session id.
func (s SessionID) String() string {
	return hex.EncodeToString(s[:])
}
