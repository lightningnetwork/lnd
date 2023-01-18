package lnwire

import (
	"encoding/binary"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// UpfrontFeePolicyRecordType is the type for upfront fees included in
	// channel updates.
	UpfrontFeePolicyRecordType tlv.Type = 1
)

// UpfrontFeePolicy is a fee policy that expresses the upfront fees for a
// channel as proportion of its the success-case fees.
type UpfrontFeePolicy struct {
	// BasePPM is the parts per million of the base fee advertised for the
	// channel charged as upfront fees per HTLC.
	BasePPM uint32

	// ProportionalPPM is the parts per million of the fee rate advertised
	// for the channel charged as upfront fees on the total htlc amount.
	ProportionalPPM uint32
}

// Record returns a TLV record for an upfront fee policy.
func (u *UpfrontFeePolicy) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		//nolint:gomnd
		UpfrontFeePolicyRecordType, &u, 8, encodeUpfrontFeePolicy,
		decodeUpfrontFeePolicy,
	)
}

// encodeUpfrontFeePolicy is an Encoder for *UpfrontFeePolicy values. An error
// is returned if val is not a **UpfrontFEePolicy.
func encodeUpfrontFeePolicy(w io.Writer, val interface{}, buf *[8]byte) error {
	if u, ok := val.(**UpfrontFeePolicy); ok {
		policy := *u

		binary.BigEndian.PutUint32(buf[:4], policy.BasePPM)
		binary.BigEndian.PutUint32(buf[4:], policy.ProportionalPPM)
		_, err := w.Write(buf[:])

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "*UpfrontFeePolicy")
}

// decodeUpfrontFeePolicy is a Decoder for *UpfrontFeePolicy values. An error
// is returned if val is not a **UpfrontFeePolicy.
func decodeUpfrontFeePolicy(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if u, ok := val.(**UpfrontFeePolicy); ok && l == 8 {
		policy := &UpfrontFeePolicy{

			BasePPM:         binary.BigEndian.Uint32(buf[:4]),
			ProportionalPPM: binary.BigEndian.Uint32(buf[4:]),
		}
		*u = policy

		return nil
	}

	//nolint:gomnd
	return tlv.NewTypeForDecodingErr(val, "*UpfrontFeePolicy", l, 8)
}
