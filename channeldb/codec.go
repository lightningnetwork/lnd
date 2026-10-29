package channeldb

import (
	"encoding/binary"
	"io"
	"time"
)

// writeElements writes fixed-size database primitives in big-endian order.
func writeElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		if err := binary.Write(w, byteOrder, element); err != nil {
			return err
		}
	}

	return nil
}

// readElements reads fixed-size database primitives in big-endian order.
func readElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		if err := binary.Read(r, byteOrder, element); err != nil {
			return err
		}
	}

	return nil
}

// deserializeTime deserializes time as unix nanoseconds.
func deserializeTime(r io.Reader) (time.Time, error) {
	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return time.Time{}, err
	}

	// Convert to time.Time. Interpret unix nano time zero as a zero
	// time.Time value.
	unixNano := byteOrder.Uint64(scratch[:])
	if unixNano == 0 {
		return time.Time{}, nil
	}

	return time.Unix(0, int64(unixNano)), nil
}

// serializeTime serializes time as unix nanoseconds.
func serializeTime(w io.Writer, t time.Time) error {
	var scratch [8]byte

	// Convert to unix nano seconds, but only if time is non-zero. Calling
	// UnixNano() on a zero time yields an undefined result.
	var unixNano int64
	if !t.IsZero() {
		unixNano = t.UnixNano()
	}

	byteOrder.PutUint64(scratch[:], uint64(unixNano))
	_, err := w.Write(scratch[:])

	return err
}
