package lnwire

import (
	"bytes"
	"encoding/binary"
	"io"
)

// PeerStorageBlob is the type of the data sent and received by peers in the
// `PeerStorage` and `PeerStorageRetrieval` message.
type PeerStorageBlob []byte

// Encode writes the PeerStorageBlob to the passed bytes.Buffer.
func (p *PeerStorageBlob) Encode(w *bytes.Buffer) error {
	// Write length first.
	var l [2]byte
	blob := *p
	binary.BigEndian.PutUint16(l[:], uint16(len(blob)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}

	// Then, write in the actual blob.
	if _, err := w.Write(blob[:]); err != nil {
		return err
	}

	return nil
}

// Decode reads the passed io.Reader into a PeerStorageBlob.
func (p *PeerStorageBlob) Decode(r io.Reader) error {
	// Read length first.
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return err
	}
	peerStorageLen := binary.BigEndian.Uint16(l[:])

	// Then read the actual blob.
	storageBlob := make(PeerStorageBlob, peerStorageLen)
	if _, err := io.ReadFull(r, storageBlob); err != nil {
		return err
	}

	*p = storageBlob

	return nil
}
