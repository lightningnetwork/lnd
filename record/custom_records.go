package record

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// CustomTypeStart is the start of the custom tlv type range as defined
	// in BOLT 01.
	CustomTypeStart = 65536
)

// CustomSet stores a set of custom key/value pairs.
type CustomSet map[uint64][]byte

// Validate checks that all custom records are in the custom type range.
func (c *CustomSet) Validate() error {
	for key := range *c {
		if key < CustomTypeStart {
			return fmt.Errorf("no custom records with types "+
				"below %v allowed", CustomTypeStart)
		}
	}

	return nil
}

// Encode writes the CustomSet to the provided io.Writer in big endian format.
func (c *CustomSet) Encode(w io.Writer) error {
	for key, value := range *c {
		// Write the map key.
		if err := binary.Write(w, binary.BigEndian, key); err != nil {
			return err
		}

		// Write the length of the map value.
		valueLength := uint16(len(value))
		err := binary.Write(w, binary.BigEndian, valueLength)
		if err != nil {
			return err
		}

		// Write the map value.
		if _, err := w.Write(value); err != nil {
			return err
		}
	}

	return nil
}

// Decode reads the CustomSet from the provided io.Reader in big endian format.
func (c *CustomSet) Decode(r io.Reader) error {
	if *c == nil {
		*c = make(CustomSet)
	}

	for {
		// Read the map key.
		var key uint64
		if err := binary.Read(r, binary.BigEndian, &key); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return err
		}

		// Read the length of the map value.
		var valueLength uint16
		err := binary.Read(r, binary.BigEndian, &valueLength)
		if err != nil {
			return err
		}

		// Read the map value.
		value := make([]byte, valueLength)
		if _, err := io.ReadFull(r, value); err != nil {
			return err
		}
		(*c)[key] = value
	}

	return nil
}
