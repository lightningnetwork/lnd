package lnwire

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// LocalNonceRecordType is the TLV type used to encode a local musig2 nonce.
	LocalNonceRecordType = 2

	// RemoteNonceRecordType is the TLV type used to encode a remote musig2
	// nonce.
	RemoteNonceRecordType = 4
)

// Musig2Nonce represents a musig2 public nonce, which is the concatenation of
// two EC points serialized in compressed format.
type Musig2Nonce [musig2.PubNonceSize]byte

// Record returns a TLV record that can be used to encode/decode the musig2
// nonce from a given TLV stream.
func (m *Musig2Nonce) record(nonceType tlv.Type) tlv.Record {
	return tlv.MakeStaticRecord(
		nonceType, m, musig2.PubNonceSize, nonceTypeEncoder,
		nonceTypeDecoder,
	)
}

// nonceTypeEncoder is a custom TLV encoder for the Musig2Nonce type.
func nonceTypeEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*Musig2Nonce); ok {
		_, err := w.Write(v[:])
		return err
	}

	fmt.Println("kek")
	return tlv.NewTypeForEncodingErr(val, "lnwire.Musig2Nonce")
}

// nonceTypeDecoder is a custom TLV decoder for the Musig2Nonce record.
func nonceTypeDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*Musig2Nonce); ok {
		_, err := io.ReadFull(r, v[:])
		return err
	}

	return tlv.NewTypeForDecodingErr(
		val, "lnwire.Musig2Nonce", l, musig2.PubNonceSize,
	)
}

// LocalMusig2Nonce is a wrapper type around the Musig2Nonce type that adds the
// directional context of a "local" nonce.
//
// TODO(roasbeef): instead have single struct?
type LocalMusig2Nonce struct {
	Musig2Nonce
}

// Record returns the record to be used for encoding a local musig2 nonce.
func (l *LocalMusig2Nonce) Record() tlv.Record {
	return l.record(LocalNonceRecordType)
}

// RemoteMusig2Nonce is a wrapper type around the Musig2Nonce type that adds
// the directional context of a "remote" nonce.
type RemoteMusig2Nonce struct {
	Musig2Nonce
}

// Record returns the record to be used for encoding a remote musig2 nonce.
func (r *RemoteMusig2Nonce) Record() tlv.Record {
	return r.record(RemoteNonceRecordType)
}
