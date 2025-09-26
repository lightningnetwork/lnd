package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// OutPoint describes an outpoint of a transaction via its transaction hash
// and the outpoint index. This is a thin wrapper around the wire.OutPoint to
// provide TLV encoding/decoding.
type OutPoint wire.OutPoint

// Record returns a TLV record that can be used to encode/decode the OutPoint.
//
// NOTE: this is part of the tlv.RecordProducer interface.
func (o *OutPoint) Record() tlv.Record {
	return tlv.MakeStaticRecord(0, o, 34, outpointEncoder, outpointDecoder)
}

// outpointEncoder is a TLV encoder for OutPoint.
func outpointEncoder(w io.Writer, val any, _ *[8]byte) error {
	if v, ok := val.(*OutPoint); ok {
		buf := bytes.NewBuffer(nil)
		err := WriteOutPoint(buf, wire.OutPoint(*v))
		if err != nil {
			return err
		}
		_, err = w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "OutPoint")
}

// outpointDecoder is a TLV decoder for OutPoint.
func outpointDecoder(r io.Reader, val any, _ *[8]byte, l uint64) error {
	if v, ok := val.(*OutPoint); ok {
		var o wire.OutPoint
		if err := ReadElement(r, &o); err != nil {
			return err
		}

		*v = OutPoint(o)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "OutPoint", l, 34)
}
