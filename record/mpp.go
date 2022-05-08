package record

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// MPPOnionType is the type used in the onion to reference the MPP fields:
// total_amt and payment_addr.
const MPPOnionType tlv.Type = 8

// MPP is a record that encodes the fields necessary for multi-path payments.
type MPP struct {
	// paymentAddr is a random, receiver-generated value used to avoid
	// collisions with concurrent payers.
	paymentAddr [32]byte

	// totalMsat is the total value of the payment, potentially spread
	// across more than one HTLC.
	totalMsat lnwire.MilliSatoshi
}

// NewMPP generates a new MPP record with the given total and payment address.
func NewMPP(total lnwire.MilliSatoshi, addr [32]byte) *MPP {
	return &MPP{
		paymentAddr: addr,
		totalMsat:   total,
	}
}

// PaymentAddr returns the payment address contained in the MPP record.
func (r *MPP) PaymentAddr() [32]byte {
	return r.paymentAddr
}

// TotalMsat returns the total value of an MPP payment in msats.
func (r *MPP) TotalMsat() lnwire.MilliSatoshi {
	return r.totalMsat
}

// MPPEncoder writes the MPP record to the provided io.Writer.
func MPPEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*MPP); ok {
		err := tlv.EBytes32(w, &v.paymentAddr, buf)
		if err != nil {
			return err
		}

		return tlv.ETUint64T(w, uint64(v.totalMsat), buf)
	}
	return tlv.NewTypeForEncodingErr(val, "MPP")
}

const (
	// minMPPLength is the minimum length of a serialized MPP TLV record,
	// which occurs when the truncated encoding of total_amt_msat takes 0
	// bytes, leaving only the payment_addr.
	minMPPLength = 32

	// maxMPPLength is the maximum length of a serialized MPP TLV record,
	// which occurs when the truncated encoding of total_amt_msat takes 8
	// bytes.
	maxMPPLength = 40
)

// MPPDecoder reads the MPP record to the provided io.Reader.
func MPPDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*MPP); ok && minMPPLength <= l && l <= maxMPPLength {
		if err := tlv.DBytes32(r, &v.paymentAddr, buf, 32); err != nil {
			return err
		}

		var total uint64
		if err := tlv.DTUint64(r, &total, buf, l-32); err != nil {
			return err
		}
		v.totalMsat = lnwire.MilliSatoshi(total)

		return nil
	}
	return tlv.NewTypeForDecodingErr(val, "MPP", l, maxMPPLength)
}

// Record returns a tlv.Record that can be used to encode or decode this record.
func (r *MPP) Record() tlv.Record {
	// Fixed-size, 32 byte payment address followed by truncated 64-bit
	// total msat.
	size := func() uint64 {
		return 32 + tlv.SizeTUint64(uint64(r.totalMsat))
	}

	return tlv.MakeDynamicRecord(
		MPPOnionType, r, size, MPPEncoder, MPPDecoder,
	)
}

// PayloadSize returns the size this record takes up in encoded form.
func (r *MPP) PayloadSize() uint64 {
	return 32 + tlv.SizeTUint64(uint64(r.totalMsat))
}

// String returns a human-readable representation of the mpp payload field.
func (r *MPP) String() string {
	if r == nil {
		return "<nil>"
	}

	return fmt.Sprintf("total=%v, addr=%x", r.totalMsat, r.paymentAddr)
}
