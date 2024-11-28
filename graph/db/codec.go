package graphdb

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
)

var (
	// byteOrder defines the preferred byte order, which is Big Endian.
	byteOrder = binary.BigEndian
)

// WriteOutpoint writes an outpoint to the passed writer using the minimal
// amount of bytes possible.
func WriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	if _, err := w.Write(o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, o.Index); err != nil {
		return err
	}

	return nil
}

// ReadOutpoint reads an outpoint from the passed reader that was previously
// written using the WriteOutpoint struct.
func ReadOutpoint(r io.Reader, o *wire.OutPoint) error {
	if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Read(r, byteOrder, &o.Index); err != nil {
		return err
	}

	return nil
}
