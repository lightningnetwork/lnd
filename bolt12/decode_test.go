package bolt12

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// appendRawRecord writes a single TLV record (type, length, value) to buf.
func appendRawRecord(t *testing.T, buf *bytes.Buffer, typ uint64,
	value []byte) {

	t.Helper()

	var scratch [8]byte
	require.NoError(t, tlv.WriteVarInt(buf, typ, &scratch))
	require.NoError(t, tlv.WriteVarInt(buf, uint64(len(value)), &scratch))
	_, err := buf.Write(value)
	require.NoError(t, err)
}

// TestDecodeRejectsNonMinimalFeatures tests that a non-minimally encoded
// feature vector is rejected at decode, so the canonical re-encode of an
// accepted message always reproduces the wire bytes.
func TestDecodeRejectsNonMinimalFeatures(t *testing.T) {
	t.Parallel()

	// A feature vector holding only bit 0 encodes minimally as 0x01. The
	// two-byte form pads it with a leading zero byte.
	padded := []byte{0x00, 0x01}

	tests := []struct {
		name   string
		typ    uint64
		decode func([]byte) error
	}{
		{
			name: "offer_features",
			typ:  12,
			decode: func(b []byte) error {
				_, err := decodeOffer(b)
				return err
			},
		},
		{
			name: "invreq_features",
			typ:  84,
			decode: func(b []byte) error {
				_, err := DecodeInvoiceRequest(b)
				return err
			},
		},
		{
			name: "invoice_features",
			typ:  174,
			decode: func(b []byte) error {
				_, err := DecodeInvoice(b)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			appendRawRecord(t, &buf, tc.typ, padded)

			err := tc.decode(buf.Bytes())
			require.ErrorIs(t, err, ErrNonMinimalFeatures)

			// The minimal encoding of the same bit set is accepted.
			var minimalBuf bytes.Buffer
			appendRawRecord(t, &minimalBuf, tc.typ, []byte{0x01})
			require.NoError(t, tc.decode(minimalBuf.Bytes()))
		})
	}
}

// TestDecodeRejectsNonMinimalAmount tests that a non-minimally encoded
// amount is rejected at decode, so the canonical re-encode of an accepted
// message always reproduces the wire bytes.
func TestDecodeRejectsNonMinimalAmount(t *testing.T) {
	t.Parallel()

	// invreq_amount (type 82) holding the value 1 in two bytes: the
	// minimal tu64 encoding of 1 is a single byte.
	var buf bytes.Buffer
	appendRawRecord(t, &buf, 82, []byte{0x00, 0x01})

	_, err := DecodeInvoiceRequest(buf.Bytes())
	require.ErrorIs(t, err, tlv.ErrTUintNotMinimal)
}

// TestUnknownOddTLVRoundTripByteExact tests that unknown odd TLV types in the
// signed range are preserved on decode and re-encode, so that the canonical
// re-encode of an accepted message always reproduces the wire bytes.
func TestUnknownOddTLVRoundTripByteExact(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	// invreq_metadata (type 0), then two unknown odd types in the signed
	// range: one with a value, one zero-length.
	appendRawRecord(t, &buf, 0, []byte("meta"))
	appendRawRecord(t, &buf, 93, []byte("xyz"))
	appendRawRecord(t, &buf, 95, nil)

	wire := buf.Bytes()

	ir, err := DecodeInvoiceRequest(wire)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, lnwire.EncodePureTLVMessage(ir, &out))
	require.Equal(t, wire, out.Bytes())
}
