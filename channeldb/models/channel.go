package models

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// serializedCircuitKeyLen is the exact length needed to encode a
	// serialized CircuitKey.
	serializedCircuitKeyLen = 16
)

var (
	// ErrInvalidCircuitKeyLen signals that a circuit key could not be
	// decoded because the byte slice is of an invalid length.
	ErrInvalidCircuitKeyLen = fmt.Errorf("length of serialized circuit " +
		"key must be 16 bytes")
)

// CircuitKey is used by a channel to uniquely identify the HTLCs it receives
// from the switch, and is used to purge our in-memory state of HTLCs that have
// already been processed by a link. Two list of CircuitKeys are included in
// each CommitDiff to allow a link to determine which in-memory htlcs directed
// the opening and closing of circuits in the switch's circuit map.
type CircuitKey struct {
	// ChanID is the short chanid indicating the HTLC's origin.
	//
	// NOTE: It is fine for this value to be blank, as this indicates a
	// locally-sourced payment.
	ChanID lnwire.ShortChannelID

	// HtlcID is the unique htlc index predominately assigned by links,
	// though can also be assigned by switch in the case of locally-sourced
	// payments.
	HtlcID uint64
}

// SetBytes deserializes the given bytes into this CircuitKey.
func (k *CircuitKey) SetBytes(bs []byte) error {
	if len(bs) != serializedCircuitKeyLen {
		return ErrInvalidCircuitKeyLen
	}

	k.ChanID = lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(bs[:8]))
	k.HtlcID = binary.BigEndian.Uint64(bs[8:])

	return nil
}

// Bytes returns the serialized bytes for this circuit key.
func (k CircuitKey) Bytes() []byte {
	bs := make([]byte, serializedCircuitKeyLen)
	binary.BigEndian.PutUint64(bs[:8], k.ChanID.ToUint64())
	binary.BigEndian.PutUint64(bs[8:], k.HtlcID)

	return bs
}

// Encode writes a CircuitKey to the provided io.Writer.
func (k *CircuitKey) Encode(w io.Writer) error {
	var scratch [serializedCircuitKeyLen]byte
	binary.BigEndian.PutUint64(scratch[:8], k.ChanID.ToUint64())
	binary.BigEndian.PutUint64(scratch[8:], k.HtlcID)

	_, err := w.Write(scratch[:])

	return err
}

// Decode reads a CircuitKey from the provided io.Reader.
func (k *CircuitKey) Decode(r io.Reader) error {
	var scratch [serializedCircuitKeyLen]byte

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	k.ChanID = lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(scratch[:8]))
	k.HtlcID = binary.BigEndian.Uint64(scratch[8:])

	return nil
}

// String returns a string representation of the CircuitKey.
func (k CircuitKey) String() string {
	return fmt.Sprintf("(Chan ID=%s, HTLC ID=%d)", k.ChanID, k.HtlcID)
}
