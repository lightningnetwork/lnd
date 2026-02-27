package lnwire

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

var (
	// ErrNilFeatureVector is returned when the supplied feature is nil.
	ErrNilFeatureVector = errors.New("cannot write nil feature vector")

	// ErrNilPublicKey is returned when a nil pubkey is used.
	ErrNilPublicKey = errors.New("cannot write nil pubkey")
)

// WriteBytes appends the given bytes to the provided buffer.
func WriteBytes(buf *bytes.Buffer, b []byte) error {
	_, err := buf.Write(b)
	return err
}

// WriteUint8 appends the uint8 to the provided buffer.
func WriteUint8(buf *bytes.Buffer, n uint8) error {
	_, err := buf.Write([]byte{n})
	return err
}

// WriteUint16 appends the uint16 to the provided buffer. It encodes the
// integer using big endian byte order.
func WriteUint16(buf *bytes.Buffer, n uint16) error {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], n)
	_, err := buf.Write(b[:])
	return err
}

// WriteUint32 appends the uint32 to the provided buffer. It encodes the
// integer using big endian byte order.
func WriteUint32(buf *bytes.Buffer, n uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	_, err := buf.Write(b[:])
	return err
}

// WriteUint64 appends the uint64 to the provided buffer. It encodes the
// integer using big endian byte order.
func WriteUint64(buf *bytes.Buffer, n uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	_, err := buf.Write(b[:])
	return err
}

// WriteSatoshi appends the Satoshi value to the provided buffer.
func WriteSatoshi(buf *bytes.Buffer, amount btcutil.Amount) error {
	return WriteUint64(buf, uint64(amount))
}

// WriteMilliSatoshi appends the MilliSatoshi value to the provided buffer.
func WriteMilliSatoshi(buf *bytes.Buffer, amount MilliSatoshi) error {
	return WriteUint64(buf, uint64(amount))
}

// WritePublicKey appends the compressed public key to the provided buffer.
func WritePublicKey(buf *bytes.Buffer, pub *btcec.PublicKey) error {
	if pub == nil {
		return ErrNilPublicKey
	}

	serializedPubkey := pub.SerializeCompressed()
	return WriteBytes(buf, serializedPubkey)
}

// WriteChannelID appends the ChannelID to the provided buffer.
func WriteChannelID(buf *bytes.Buffer, channelID ChannelID) error {
	return WriteBytes(buf, channelID[:])
}

// WriteShortChannelID appends the ShortChannelID to the provided buffer. It
// encodes the BlockHeight and TxIndex each using 3 bytes with big endian byte
// order, and encodes txPosition using 2 bytes with big endian byte order.
func WriteShortChannelID(buf *bytes.Buffer, shortChanID ShortChannelID) error {
	// Check that field fit in 3 bytes and write the blockHeight
	if shortChanID.BlockHeight > ((1 << 24) - 1) {
		return errors.New("block height should fit in 3 bytes")
	}

	var blockHeight [4]byte
	binary.BigEndian.PutUint32(blockHeight[:], shortChanID.BlockHeight)

	if _, err := buf.Write(blockHeight[1:]); err != nil {
		return err
	}

	// Check that field fit in 3 bytes and write the txIndex
	if shortChanID.TxIndex > ((1 << 24) - 1) {
		return errors.New("tx index should fit in 3 bytes")
	}

	var txIndex [4]byte
	binary.BigEndian.PutUint32(txIndex[:], shortChanID.TxIndex)
	if _, err := buf.Write(txIndex[1:]); err != nil {
		return err
	}

	// Write the TxPosition
	return WriteUint16(buf, shortChanID.TxPosition)
}

// WriteSig appends the signature to the provided buffer.
func WriteSig(buf *bytes.Buffer, sig Sig) error {
	return WriteBytes(buf, sig.bytes[:])
}

// WriteSigs appends the slice of signatures to the provided buffer with its
// length.
func WriteSigs(buf *bytes.Buffer, sigs []Sig) error {
	// Write the length of the sigs.
	if err := WriteUint16(buf, uint16(len(sigs))); err != nil {
		return err
	}

	for _, sig := range sigs {
		if err := WriteSig(buf, sig); err != nil {
			return err
		}
	}
	return nil
}

// WriteFailCode appends the FailCode to the provided buffer.
func WriteFailCode(buf *bytes.Buffer, e FailCode) error {
	return WriteUint16(buf, uint16(e))
}

// WriteRawFeatureVector encodes the feature using the feature's Encode method
// and appends the data to the provided buffer. An error will return if the
// passed feature is nil.
func WriteRawFeatureVector(buf *bytes.Buffer, feature *RawFeatureVector) error {
	if feature == nil {
		return ErrNilFeatureVector
	}

	return feature.Encode(buf)
}

// WriteChanUpdateMsgFlags appends the update flag to the provided buffer.
func WriteChanUpdateMsgFlags(buf *bytes.Buffer, f ChanUpdateMsgFlags) error {
	return WriteUint8(buf, uint8(f))
}

// WriteChanUpdateChanFlags appends the update flag to the provided buffer.
func WriteChanUpdateChanFlags(buf *bytes.Buffer, f ChanUpdateChanFlags) error {
	return WriteUint8(buf, uint8(f))
}

// WriteErrorData appends the data to the provided buffer.
func WriteErrorData(buf *bytes.Buffer, data ErrorData) error {
	return writeDataWithLength(buf, data)
}

// WriteBool appends the boolean to the provided buffer.
func WriteBool(buf *bytes.Buffer, b bool) error {
	if b {
		return WriteBytes(buf, []byte{1})
	}
	return WriteBytes(buf, []byte{0})
}

// writeDataWithLength writes the data and its length to the buffer.
func writeDataWithLength(buf *bytes.Buffer, data []byte) error {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(data)))
	if _, err := buf.Write(l[:]); err != nil {
		return err
	}

	_, err := buf.Write(data)
	return err
}
