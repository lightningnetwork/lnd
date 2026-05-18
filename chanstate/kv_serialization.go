package chanstate

import (
	"encoding/binary"
	"io"
)

var byteOrder = binary.BigEndian

// serializeLogUpdate writes a log update to the provided io.Writer.
func serializeLogUpdate(w io.Writer, l *LogUpdate) error {
	return WriteElements(w, l.LogIndex, l.UpdateMsg)
}

// deserializeLogUpdate reads a log update from the provided io.Reader.
func deserializeLogUpdate(r io.Reader) (*LogUpdate, error) {
	l := &LogUpdate{}
	if err := ReadElements(r, &l.LogIndex, &l.UpdateMsg); err != nil {
		return nil, err
	}

	return l, nil
}

// SerializeLogUpdates serializes provided list of updates to a stream.
func SerializeLogUpdates(w io.Writer, logUpdates []LogUpdate) error {
	numUpdates := uint16(len(logUpdates))
	if err := binary.Write(w, byteOrder, numUpdates); err != nil {
		return err
	}

	for _, diff := range logUpdates {
		err := WriteElements(w, diff.LogIndex, diff.UpdateMsg)
		if err != nil {
			return err
		}
	}

	return nil
}

// DeserializeLogUpdates deserializes a list of updates from a stream.
func DeserializeLogUpdates(r io.Reader) ([]LogUpdate, error) {
	var numUpdates uint16
	if err := binary.Read(r, byteOrder, &numUpdates); err != nil {
		return nil, err
	}

	logUpdates := make([]LogUpdate, numUpdates)
	for i := 0; i < int(numUpdates); i++ {
		err := ReadElements(r,
			&logUpdates[i].LogIndex, &logUpdates[i].UpdateMsg,
		)
		if err != nil {
			return nil, err
		}
	}

	return logUpdates, nil
}

// makeLogKey converts a uint64 into an 8 byte array.
func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}
