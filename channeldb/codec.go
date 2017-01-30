package channeldb

import (
	"encoding/binary"
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"io"
)

// deserialize function is used to convert object into bytes representation
// and store in database.
func deserialize(obj interface{}, r io.Reader) error {
	switch e := obj.(type) {
	case *wire.OutPoint:
		var h [32]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return err
		}

		hash, err := chainhash.NewHash(h[:])
		if err != nil {
			return err
		}

		// Index
		var idxBytes [4]byte
		_, err = io.ReadFull(r, idxBytes[:])
		if err != nil {
			return err
		}
		index := binary.BigEndian.Uint32(idxBytes[:])

		e.Hash = *hash
		e.Index = index

	case *PaymentCircuit:
		e.Dest = &wire.OutPoint{}
		if err := deserialize(e.Dest, r); err != nil {
			return err
		}

		e.Src = &wire.OutPoint{}
		if err := deserialize(e.Src, r); err != nil {
			return err
		}

		if _, err := r.Read(e.PaymentHash[:]); err != nil {
			return err
		}

	default:
		return errors.New("unknown type of message")
	}

	return nil
}

// serialize is used to restore the object from bytes representation.
func serialize(obj interface{}, w io.Writer) error {
	switch e := obj.(type) {
	case *wire.OutPoint:
		// First write out the previous txid.
		var h [32]byte
		copy(h[:], e.Hash[:])
		if _, err := w.Write(h[:]); err != nil {
			return err
		}

		// Then the exact index of this output.
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], e.Index)
		if _, err := w.Write(idx[:]); err != nil {
			return err
		}

	case *PaymentCircuit:
		if err := serialize(e.Dest, w); err != nil {
			return err
		}

		if err := serialize(e.Src, w); err != nil {
			return err
		}

		if _, err := w.Write(e.PaymentHash[:]); err != nil {
			return err
		}

	default:
		return errors.New("unknown type of message")
	}

	return nil
}
