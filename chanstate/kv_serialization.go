package chanstate

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
)

var byteOrder = binary.BigEndian

// serializeLogUpdate writes a log update to the provided io.Writer.
func serializeLogUpdate(w io.Writer, l *LogUpdate) error {
	if err := binary.Write(w, byteOrder, l.LogIndex); err != nil {
		return err
	}

	var msgBuf bytes.Buffer
	if _, err := lnwire.WriteMessage(&msgBuf, l.UpdateMsg, 0); err != nil {
		return err
	}

	msgLen := uint16(msgBuf.Len())
	if err := binary.Write(w, byteOrder, msgLen); err != nil {
		return err
	}

	_, err := w.Write(msgBuf.Bytes())

	return err
}

// deserializeLogUpdate reads a log update from the provided io.Reader.
func deserializeLogUpdate(r io.Reader) (*LogUpdate, error) {
	l := &LogUpdate{}
	if err := binary.Read(r, byteOrder, &l.LogIndex); err != nil {
		return nil, err
	}

	var msgLen uint16
	if err := binary.Read(r, byteOrder, &msgLen); err != nil {
		return nil, err
	}

	msgReader := io.LimitReader(r, int64(msgLen))
	msg, err := lnwire.ReadMessage(msgReader, 0)
	if err != nil {
		return nil, err
	}

	l.UpdateMsg = msg

	return l, nil
}

// makeLogKey converts a uint64 into an 8 byte array.
func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}
