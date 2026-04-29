package bolt12

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// someErrField builds a set erroneous_field record for the given field
// number, keeping the test tables compact.
func someErrField(n uint64) tlv.OptionalRecordT[tlv.TlvType1, TUint64] {
	return tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType1](TUint64(n)),
	)
}

// someSuggested builds a set suggested_value record from raw bytes.
func someSuggested(b tlv.Blob) tlv.OptionalRecordT[tlv.TlvType3, tlv.Blob] {
	return tlv.SomeRecordT(tlv.NewPrimitiveRecord[tlv.TlvType3](b))
}

// someError builds a set error record from a string.
func someError(s string) tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob] {
	return tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType5](tlv.Blob(s)),
	)
}

// TestInvoiceErrorRoundTrip verifies that encoding an invoice_error and
// decoding the result recovers every field, and that re-encoding the decoded
// message reproduces the original bytes, both for a fully-populated message
// and for the minimal error-only case. Note that this round-trip property
// only guarantees exact byte reproducibility for messages containing only
// known/declared fields; any unknown fields present in decoded messages are
// intentionally dropped when re-encoded.
func TestInvoiceErrorRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ie           *InvoiceError
		wantMsg      string
		wantHasField bool
		wantFieldNum uint64
		wantSuggest  []byte
	}{
		{
			name: "all fields",
			ie: &InvoiceError{
				ErroneousField: someErrField(82),
				SuggestedValue: someSuggested(
					[]byte{0x00, 0x01, 0x86, 0xa0},
				),
				Error: someError("amount too low"),
			},
			wantMsg:      "amount too low",
			wantHasField: true,
			wantFieldNum: 82,
			wantSuggest:  []byte{0x00, 0x01, 0x86, 0xa0},
		},
		{
			name: "minimal error only",
			ie: &InvoiceError{
				Error: someError("rejected"),
			},
			wantMsg:      "rejected",
			wantHasField: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := tc.ie.Encode()
			require.NoError(t, err)
			require.NotEmpty(t, encoded)

			decoded, err := DecodeInvoiceError(encoded)
			require.NoError(t, err)

			require.Equal(t, tc.wantMsg, decoded.ErrorMessage())

			fieldNum, ok := decoded.FieldNumber()
			require.Equal(t, tc.wantHasField, ok)
			if tc.wantHasField {
				require.Equal(t, tc.wantFieldNum, fieldNum)
			}

			var sugVal []byte
			decoded.SuggestedValue.WhenSome(
				func(r tlv.RecordT[tlv.TlvType3, tlv.Blob]) {
					sugVal = r.Val
				},
			)
			require.Equal(t, tc.wantSuggest, sugVal)

			// Re-encoding the decoded message must reproduce the
			// original bytes, pinning canonical record ordering.
			reencoded, err := decoded.Encode()
			require.NoError(t, err)
			require.Equal(t, encoded, reencoded)
		})
	}
}

// TestInvoiceErrorRoundTripWithUnknown verifies that decoding an invoice_error
// containing unknown odd fields works, but re-encoding the decoded structure
// drops those unknown fields, yielding only the known fields in the encoded
// byte stream.
func TestInvoiceErrorRoundTripWithUnknown(t *testing.T) {
	t.Parallel()

	// Create a valid invoice_error with only known fields and encode it.
	ie := &InvoiceError{
		Error: someError("rejected with unknown field present"),
	}
	valid, err := ie.Encode()
	require.NoError(t, err)

	// Append an unknown odd TLV (type 7) to the valid TLV stream.
	// 0x07 (type), 0x02 (length), 0xaa, 0xbb (value).
	streamWithUnknown := append(
		append([]byte{}, valid...), 0x07, 0x02, 0xaa, 0xbb,
	)

	// Decode the stream. It should succeed because unknown odd fields are
	// ignored/tolerated.
	decoded, err := DecodeInvoiceError(streamWithUnknown)
	require.NoError(t, err)
	require.Equal(
		t, "rejected with unknown field present",
		decoded.ErrorMessage(),
	)

	// Re-encode the decoded message.
	reencoded, err := decoded.Encode()
	require.NoError(t, err)

	// The re-encoded stream must drop the unknown type 7 field, recovering
	// exactly the 'valid' bytes, rather than 'streamWithUnknown'.
	require.Equal(t, valid, reencoded)
}

// TestInvoiceErrorEncodeValidates verifies that Encode runs the writer
// validation before serialising, so an invalid invoice_error never reaches the
// wire.
func TestInvoiceErrorEncodeValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ie      *InvoiceError
		wantErr error
	}{
		{
			name:    "missing error",
			ie:      &InvoiceError{},
			wantErr: ErrMissingError,
		},
		{
			name:    "empty error",
			ie:      &InvoiceError{Error: someError("")},
			wantErr: ErrEmptyError,
		},
		{
			name: "non-utf8 error",
			ie: &InvoiceError{
				Error: someError(string([]byte{0xff, 0xfe})),
			},
			wantErr: ErrInvalidUTF8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := tc.ie.Encode()
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestDecodeInvoiceError verifies decode-level behavior: a truncated stream
// errors, and an unknown odd TLV trailing a valid message is tolerated per the
// BOLT rule that unknown odd types may be ignored.
func TestDecodeInvoiceError(t *testing.T) {
	t.Parallel()

	valid, err := (&InvoiceError{Error: someError("rejected")}).Encode()
	require.NoError(t, err)

	// A valid message with an unknown odd TLV (type 7) appended after error
	// (type 5), kept in ascending type order.
	withOdd := append(append([]byte{}, valid...), 0x07, 0x02, 0xaa, 0xbb)

	tests := []struct {
		name    string
		data    []byte
		wantErr bool
		wantMsg string
	}{
		{
			// Type 5 (error) claims length 16 but supplies one
			// byte.
			name:    "truncated",
			data:    []byte{0x05, 0x10, 0x01},
			wantErr: true,
		},
		{
			name:    "unknown odd tolerated",
			data:    withOdd,
			wantMsg: "rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			decoded, err := DecodeInvoiceError(tc.data)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantMsg, decoded.ErrorMessage())
		})
	}
}

// TestValidateInvoiceErrorRead verifies the BOLT 1 must-understand rule: an
// unknown even TLV is rejected (including a zero-length one, which still
// occupies a type slot), while an unknown odd TLV is tolerated.
func TestValidateInvoiceErrorRead(t *testing.T) {
	t.Parallel()

	// A valid encoded invoice_error (error = "rejected", type 5). Trailers
	// use types > 5 to keep the stream strictly increasing.
	base, err := (&InvoiceError{Error: someError("rejected")}).Encode()
	require.NoError(t, err)

	tests := []struct {
		name    string
		trailer []byte
		wantErr error
	}{
		{
			name: "known only",
		},
		{
			name:    "unknown odd tolerated",
			trailer: []byte{0x07, 0x02, 0xaa, 0xbb},
		},
		{
			name:    "unknown even rejected",
			trailer: []byte{0x06, 0x02, 0xaa, 0xbb},
			wantErr: ErrUnknownEvenType,
		},
		{
			name:    "unknown even zero-length rejected",
			trailer: []byte{0x06, 0x00},
			wantErr: ErrUnknownEvenType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := append(
				append([]byte{}, base...), tc.trailer...,
			)
			decoded, err := DecodeInvoiceError(stream)
			require.NoError(t, err)

			err = ValidateInvoiceErrorRead(decoded)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
