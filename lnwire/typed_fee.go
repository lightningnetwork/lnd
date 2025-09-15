package lnwire

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	FeeRecordType tlv.Type = 55555
)

// Fee represents a fee schedule.
type Fee struct {
	BaseFee int32
	FeeRate int32
}

// Record returns a TLV record that can be used to encode/decode the fee
// type from a given TLV stream.
func (l *Fee) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		FeeRecordType, l, 8, feeEncoder, feeDecoder,
	)
}

// feeEncoder is a custom TLV encoder for the fee record.
func feeEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	v, ok := val.(*Fee)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "lnwire.Fee")
	}

	if err := tlv.EUint32T(w, uint32(v.BaseFee), buf); err != nil {
		return err
	}

	return tlv.EUint32T(w, uint32(v.FeeRate), buf)
}

// feeDecoder is a custom TLV decoder for the fee record.
func feeDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	v, ok := val.(*Fee)
	if !ok {
		return tlv.NewTypeForDecodingErr(val, "lnwire.Fee", l, 8)
	}

	var baseFee, feeRate uint32
	if err := tlv.DUint32(r, &baseFee, buf, 4); err != nil {
		return err
	}
	if err := tlv.DUint32(r, &feeRate, buf, 4); err != nil {
		return err
	}

	v.FeeRate = int32(feeRate)
	v.BaseFee = int32(baseFee)

	return nil
}
