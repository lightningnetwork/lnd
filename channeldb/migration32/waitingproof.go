package migration32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
)

var byteOrder = binary.BigEndian

// WaitingProofType represents the type of the encoded waiting proof.
type WaitingProofType uint8

const (
	// WaitingProofTypeLegacy represents a waiting proof for legacy P2WSH
	// channels.
	WaitingProofTypeLegacy WaitingProofType = 0
)

// WaitingProofKey is the proof key which uniquely identifies the waiting
// proof object. The goal of this key is distinguish the local and remote
// proof for the same channel id.
type WaitingProofKey [9]byte

// WaitingProof is the storable object, which encapsulate the half proof and
// the information about from which side this proof came. This structure is
// needed to make channel proof exchange persistent, so that after client
// restart we may receive remote/local half proof and process it.
type WaitingProof struct {
	*lnwire.AnnounceSignatures
	isRemote bool
}

// Key returns the key which uniquely identifies waiting proof.
func (p *WaitingProof) Key() WaitingProofKey {
	var key [9]byte
	binary.BigEndian.PutUint64(key[:8], p.ShortChannelID.ToUint64())

	if p.isRemote {
		key[8] = 1
	}
	return key
}

// UpdatedEncode writes the internal representation of waiting proof in byte
// stream using the new format that is prefixed with a type byte.
func (p *WaitingProof) UpdatedEncode(w io.Writer) error {
	// Write the type byte.
	err := binary.Write(w, byteOrder, WaitingProofTypeLegacy)
	if err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.isRemote); err != nil {
		return err
	}

	buf, ok := w.(*bytes.Buffer)
	if !ok {
		return fmt.Errorf("expect io.Writer to be *bytes.Buffer")
	}

	if err := p.AnnounceSignatures.Encode(buf, 0); err != nil {
		return err
	}

	return nil
}

// LegacyEncode writes the internal representation of waiting proof in byte
// stream using the legacy format.
func (p *WaitingProof) LegacyEncode(w io.Writer) error {
	if err := binary.Write(w, byteOrder, p.isRemote); err != nil {
		return err
	}

	buf, ok := w.(*bytes.Buffer)
	if !ok {
		return fmt.Errorf("expect io.Writer to be *bytes.Buffer")
	}

	if err := p.AnnounceSignatures.Encode(buf, 0); err != nil {
		return err
	}

	return nil
}

// LegacyDecode reads the data from the byte stream and initializes the
// waiting proof object with it.
func (p *WaitingProof) LegacyDecode(r io.Reader) error {
	if err := binary.Read(r, byteOrder, &p.isRemote); err != nil {
		return err
	}

	msg := &lnwire.AnnounceSignatures{}
	if err := msg.Decode(r, 0); err != nil {
		return err
	}

	(*p).AnnounceSignatures = msg
	return nil
}
