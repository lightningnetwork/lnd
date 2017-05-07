package lnwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// WriteOutPoint serializes a wire.OutPoint struct into the passed io.Writer
// stream.
func WriteOutPoint(w io.Writer, o *wire.OutPoint) error {
	if _, err := w.Write(o.Hash[:chainhash.HashSize]); err != nil {
		return err
	}

	if o.Index > math.MaxUint16 {
		return fmt.Errorf("index for outpoint (%v) is "+
			"greater than max index of %v", o.Index, math.MaxUint16)
	}

	var idx [2]byte
	binary.BigEndian.PutUint16(idx[:], uint16(o.Index))
	if _, err := w.Write(idx[:]); err != nil {
		return err
	}

	return nil
}

// ReadOutPoint deserializes a wire.OutPoint struct from the passed io.Reader
// stream.
func ReadOutPoint(r io.Reader, o *wire.OutPoint) error {
	var h [chainhash.HashSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	hash, err := chainhash.NewHash(h[:])
	if err != nil {
		return err
	}

	var idxBytes [2]byte
	_, err = io.ReadFull(r, idxBytes[:])
	if err != nil {
		return err
	}
	index := binary.BigEndian.Uint16(idxBytes[:])

	*o = wire.OutPoint{
		Hash:  *hash,
		Index: uint32(index),
	}

	return nil
}
