package funding

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
)

var (
	// byteOrder defines the endian-ness we use for encoding to and from
	// buffers.
	byteOrder = binary.BigEndian
)

// WriteOutpoint writes an outpoint to an io.Writer. This is not the same as
// the channeldb variant as this uses WriteVarBytes for the Hash.
func WriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}
