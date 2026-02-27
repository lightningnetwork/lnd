package lnwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

const (
	// MaxSliceLength is the maximum allowed length for any opaque byte
	// slices in the wire protocol.
	MaxSliceLength = 65535

	// MaxMsgBody is the largest payload any message is allowed to provide.
	// This is two less than the MaxSliceLength as each message has a 2
	// byte type that precedes the message body.
	MaxMsgBody = 65533
)

// PkScript is simple type definition which represents a raw serialized public
// key script.
type PkScript []byte

// WriteElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized for the wire protocol.
//
// TODO(yy): rm this method once we finish dereferencing it from other
// packages.
func WriteElement(w *bytes.Buffer, element interface{}) error {
	switch e := element.(type) {
	case uint8:
		var b [1]byte
		b[0] = e
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint16:
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ChanUpdateMsgFlags:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ChanUpdateChanFlags:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case MilliSatoshi:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case btcutil.Amount:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint32:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case *btcec.PublicKey:
		if e == nil {
			return fmt.Errorf("cannot write nil pubkey")
		}

		var b [33]byte
		serializedPubkey := e.SerializeCompressed()
		copy(b[:], serializedPubkey)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case []Sig:
		var b [2]byte
		numSigs := uint16(len(e))
		binary.BigEndian.PutUint16(b[:], numSigs)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

		for _, sig := range e {
			if err := WriteElement(w, sig); err != nil {
				return err
			}
		}

	case Sig:
		// Write buffer
		if _, err := w.Write(e.bytes[:]); err != nil {
			return err
		}

	case ErrorData:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case [33]byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case []byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case *RawFeatureVector:
		if e == nil {
			return fmt.Errorf("cannot write nil feature vector")
		}

		if err := e.Encode(w); err != nil {
			return err
		}

	case ChannelID:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case FailCode:
		if err := WriteElement(w, uint16(e)); err != nil {
			return err
		}

	case ShortChannelID:
		// Check that field fit in 3 bytes and write the blockHeight
		if e.BlockHeight > ((1 << 24) - 1) {
			return errors.New("block height should fit in 3 bytes")
		}

		var blockHeight [4]byte
		binary.BigEndian.PutUint32(blockHeight[:], e.BlockHeight)

		if _, err := w.Write(blockHeight[1:]); err != nil {
			return err
		}

		// Check that field fit in 3 bytes and write the txIndex
		if e.TxIndex > ((1 << 24) - 1) {
			return errors.New("tx index should fit in 3 bytes")
		}

		var txIndex [4]byte
		binary.BigEndian.PutUint32(txIndex[:], e.TxIndex)
		if _, err := w.Write(txIndex[1:]); err != nil {
			return err
		}

		// Write the txPosition
		var txPosition [2]byte
		binary.BigEndian.PutUint16(txPosition[:], e.TxPosition)
		if _, err := w.Write(txPosition[:]); err != nil {
			return err
		}

	case bool:
		var b [1]byte
		if e {
			b[0] = 1
		}
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ExtraOpaqueData:
		return e.Encode(w)

	default:
		return fmt.Errorf("unknown type in WriteElement: %T", e)
	}

	return nil
}

// WriteElements is writes each element in the elements slice to the passed
// buffer using WriteElement.
//
// TODO(yy): rm this method once we finish dereferencing it from other
// packages.
func WriteElements(buf *bytes.Buffer, elements ...interface{}) error {
	for _, element := range elements {
		err := WriteElement(buf, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadElement is a one-stop utility function to deserialize any datastructure
// encoded using the serialization format of lnwire.
func ReadElement(r io.Reader, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *bool:
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}

		if b[0] == 1 {
			*e = true
		}

	case *uint8:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = b[0]

	case *uint16:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint16(b[:])

	case *ChanUpdateMsgFlags:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = ChanUpdateMsgFlags(b[0])

	case *ChanUpdateChanFlags:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = ChanUpdateChanFlags(b[0])

	case *uint32:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint32(b[:])

	case *uint64:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint64(b[:])

	case *MilliSatoshi:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = MilliSatoshi(int64(binary.BigEndian.Uint64(b[:])))

	case *btcutil.Amount:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))

	case **btcec.PublicKey:
		var b [btcec.PubKeyBytesLenCompressed]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return err
		}

		pubKey, err := btcec.ParsePubKey(b[:])
		if err != nil {
			return err
		}
		*e = pubKey

	case *RawFeatureVector:
		f := NewRawFeatureVector()
		err = f.Decode(r)
		if err != nil {
			return err
		}
		*e = *f

	case **RawFeatureVector:
		f := NewRawFeatureVector()
		err = f.Decode(r)
		if err != nil {
			return err
		}
		*e = f

	case *[]Sig:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		numSigs := binary.BigEndian.Uint16(l[:])

		var sigs []Sig
		if numSigs > 0 {
			sigs = make([]Sig, numSigs)
			for i := 0; i < int(numSigs); i++ {
				if err := ReadElement(r, &sigs[i]); err != nil {
					return err
				}
			}
		}
		*e = sigs

	case *Sig:
		if _, err := io.ReadFull(r, e.bytes[:]); err != nil {
			return err
		}

	case *ErrorData:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		errorLen := binary.BigEndian.Uint16(l[:])

		*e = ErrorData(make([]byte, errorLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *[33]byte:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case []byte:
		if _, err := io.ReadFull(r, e); err != nil {
			return err
		}

	case *FailCode:
		if err := ReadElement(r, (*uint16)(e)); err != nil {
			return err
		}

	case *ChannelID:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *ShortChannelID:
		var blockHeight [4]byte
		if _, err = io.ReadFull(r, blockHeight[1:]); err != nil {
			return err
		}

		var txIndex [4]byte
		if _, err = io.ReadFull(r, txIndex[1:]); err != nil {
			return err
		}

		var txPosition [2]byte
		if _, err = io.ReadFull(r, txPosition[:]); err != nil {
			return err
		}

		*e = ShortChannelID{
			BlockHeight: binary.BigEndian.Uint32(blockHeight[:]),
			TxIndex:     binary.BigEndian.Uint32(txIndex[:]),
			TxPosition:  binary.BigEndian.Uint16(txPosition[:]),
		}

	case *ExtraOpaqueData:
		return e.Decode(r)

	default:
		return fmt.Errorf("unknown type in ReadElement: %T", e)
	}

	return nil
}

// ReadElements deserializes a variable number of elements into the passed
// io.Reader, with each element being deserialized according to the ReadElement
// function.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		err := ReadElement(r, element)
		if err != nil {
			return err
		}
	}
	return nil
}
