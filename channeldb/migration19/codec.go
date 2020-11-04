package migration19

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
)

var (
	byteOrder = binary.BigEndian
)

// legacyWriteOutpoint is the legacy write encoding for outpoints.
func legacyWriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}

// legacyReadOutpoint is the legacy read encoding for outpoints.
func legacyReadOutpoint(r io.Reader, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	txid, err := wire.ReadVarBytes(r, 0, 32, "prevout")
	if err != nil {
		return err
	}
	copy(o.Hash[:], txid)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	o.Index = byteOrder.Uint32(scratch)

	return nil
}

// channeldbWriteOutpoint is a copy of the channeldb version of
// WriteOutpoint.
func channeldbWriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	if _, err := w.Write(o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, o.Index); err != nil {
		return err
	}

	return nil
}
