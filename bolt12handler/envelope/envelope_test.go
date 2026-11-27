package envelope

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestRoundTrip verifies that decoding an encoded envelope returns the same
// data for any field values, with and without the optional records.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		data := &Data{
			PayerID: [33]byte(rapid.SliceOfN(
				rapid.Byte(), 33, 33,
			).Draw(t, "payerID")),
			CreatedAt: rapid.Uint64().Draw(t, "createdAt"),
			Amount:    rapid.Uint64().Draw(t, "amount"),
			Quantity: rapid.OneOf(
				rapid.Just(uint64(0)), rapid.Uint64(),
			).Draw(t, "quantity"),
		}
		if rapid.Bool().Draw(t, "withOfferHash") {
			data.OfferHash = fn.Some([32]byte(rapid.SliceOfN(
				rapid.Byte(), 32, 32,
			).Draw(t, "offerHash")))
		}
		note := rapid.SliceOfN(
			rapid.Byte(), 0, MaxPayerNoteLen,
		).Draw(t, "payerNote")
		if len(note) > 0 {
			data.PayerNote = note
		}

		encoded, err := Encode(data)
		require.NoError(t, err)

		decoded, err := Decode(encoded)
		require.NoError(t, err)
		require.Equal(t, data, decoded)
	})
}

// TestOptionalRecords verifies that a zero quantity, an empty payer note and a
// missing offer hash are left out of the encoding.
func TestOptionalRecords(t *testing.T) {
	t.Parallel()

	data := &Data{CreatedAt: 1, Amount: 2}
	bare, err := Encode(data)
	require.NoError(t, err)

	// The payer id, created_at and amount records only.
	require.Len(t, bare, 2+33+2+1+2+1)

	data.Quantity = 1
	data.PayerNote = []byte("hi")
	data.OfferHash = fn.Some([32]byte{1})
	full, err := Encode(data)
	require.NoError(t, err)
	require.Len(t, full, len(bare)+(2+32)+(2+1)+(2+2))

	decoded, err := Decode(bare)
	require.NoError(t, err)
	require.True(t, decoded.OfferHash.IsNone())
	require.Nil(t, decoded.PayerNote)
	require.Zero(t, decoded.Quantity)
}

// TestPayerNoteCap verifies that Encode refuses a payer note over the cap, so
// no caller can make an envelope over the onion budget.
func TestPayerNoteCap(t *testing.T) {
	t.Parallel()

	data := &Data{
		CreatedAt: 1,
		Amount:    2,
		PayerNote: bytes.Repeat([]byte{'a'}, MaxPayerNoteLen),
	}
	_, err := Encode(data)
	require.NoError(t, err)

	data.PayerNote = append(data.PayerNote, 'a')
	_, err = Encode(data)
	require.ErrorIs(t, err, ErrPayerNoteTooLong)
}

// TestSize pins the size of an invoice envelope with large field values,
// because padding repeats it on every blinded hop.
func TestSize(t *testing.T) {
	t.Parallel()

	// An amount just below 2^40 msat is about 11 BTC.
	encoded, err := Encode(&Data{
		OfferHash: fn.Some([32]byte{}),
		CreatedAt: 1_800_000_000,
		Amount:    1<<40 - 1,
		Quantity:  100,
		PayerNote: bytes.Repeat([]byte{'a'}, MaxPayerNoteLen),
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 151)
}

// encodeRecords builds raw envelope bytes from the given records. It can build
// envelopes that Encode cannot produce, such as ones with a missing or short
// field.
func encodeRecords(t *testing.T, records ...tlv.Record) []byte {
	t.Helper()

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, stream.Encode(&buf))

	return buf.Bytes()
}

// TestDecodeErrors verifies that decoding refuses data with a missing required
// field, a field of the wrong length or a broken stream.
func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	var (
		offerHash = make([]byte, 32)
		payerID   = make([]byte, 33)
		short     = make([]byte, 31)
		longNote  = make([]byte, MaxPayerNoteLen+1)
		createdAt = uint64(1)
		amount    = uint64(1)
	)
	record := tlv.MakePrimitiveRecord
	tu64 := func(typ tlv.Type, val *uint64) tlv.Record {
		return tlv.MakeDynamicRecord(
			typ, val, func() uint64 {
				return tlv.SizeTUint64(*val)
			}, tlv.ETUint64, tlv.DTUint64,
		)
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			name:    "empty",
			wantErr: "missing payer_id",
		},
		{
			name: "missing payer id",
			data: encodeRecords(
				t,
				record(typeOfferHash, &offerHash),
				tu64(typeCreatedAt, &createdAt),
				tu64(typeAmount, &amount),
			),
			wantErr: "missing payer_id",
		},
		{
			name: "missing created at",
			data: encodeRecords(
				t,
				record(typePayerID, &payerID),
				tu64(typeAmount, &amount),
			),
			wantErr: "missing created_at",
		},
		{
			name: "missing amount",
			data: encodeRecords(
				t,
				record(typePayerID, &payerID),
				tu64(typeCreatedAt, &createdAt),
			),
			wantErr: "missing amount",
		},
		{
			name: "short offer hash",
			data: encodeRecords(
				t,
				record(typeOfferHash, &short),
				record(typePayerID, &payerID),
				tu64(typeCreatedAt, &createdAt),
				tu64(typeAmount, &amount),
			),
			wantErr: "invalid offer_hash length",
		},
		{
			name: "short payer id",
			data: encodeRecords(
				t,
				record(typePayerID, &short),
				tu64(typeCreatedAt, &createdAt),
				tu64(typeAmount, &amount),
			),
			wantErr: "invalid payer_id length",
		},
		{
			name: "long payer note",
			data: encodeRecords(
				t,
				record(typePayerID, &payerID),
				tu64(typeCreatedAt, &createdAt),
				tu64(typeAmount, &amount),
				record(typePayerNote, &longNote),
			),
			wantErr: "payer note too long",
		},
		{
			name:    "truncated stream",
			data:    []byte{0x02, 0x21, 0x01},
			wantErr: "decode tlv",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(tc.data)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
