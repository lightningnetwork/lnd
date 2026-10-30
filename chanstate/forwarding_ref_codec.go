package chanstate

import (
	"encoding/binary"
	"io"
)

// Encode serializes the AddRef to the given io.Writer.
func (a *AddRef) Encode(w io.Writer) error {
	if err := binary.Write(w, binary.BigEndian, a.Height); err != nil {
		return err
	}

	return binary.Write(w, binary.BigEndian, a.Index)
}

// Decode deserializes the AddRef from the given io.Reader.
func (a *AddRef) Decode(r io.Reader) error {
	if err := binary.Read(r, binary.BigEndian, &a.Height); err != nil {
		return err
	}

	return binary.Read(r, binary.BigEndian, &a.Index)
}
