package bolt12

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestBech32FormatStringVectors runs through every test case in the spec's
// format-string-test.json to verify our bech32 encoder/decoder handles
// continuations, case, and edge cases correctly.
func TestBech32FormatStringVectors(t *testing.T) {
	t.Parallel()

	vectors := loadFormatStringVectors(t)
	require.NotEmpty(t, vectors)

	for _, tc := range vectors {
		t.Run(tc.Comment, func(t *testing.T) {
			t.Parallel()

			hrp, decoded, err := Decode(tc.String)

			if !tc.Valid {
				require.Error(t, err, "expected error for: %s",
					tc.Comment)

				return
			}

			require.NoError(t, err, "unexpected error for: %s",
				tc.Comment)
			require.Equal(t, HRPOffer, hrp)
			require.NotEmpty(t, decoded)

			// Round-trip: re-encode and decode again.
			encoded, err := Encode(hrp, decoded)
			require.NoError(t, err)

			hrp2, decoded2, err := Decode(encoded)
			require.NoError(t, err)
			require.Equal(t, hrp, hrp2)
			require.Equal(t, decoded, decoded2)
		})
	}
}

// TestBech32RoundTrip verifies that encoding then decoding returns the original
// data for each supported HRP.
func TestBech32RoundTrip(t *testing.T) {
	t.Parallel()

	testData := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd}

	for _, hrp := range []string{HRPOffer, HRPInvoiceRequest, HRPInvoice} {
		t.Run(hrp, func(t *testing.T) {
			t.Parallel()

			encoded, err := Encode(hrp, testData)
			require.NoError(t, err)
			require.True(t, len(encoded) > len(hrp)+1)

			gotHRP, gotData, err := Decode(encoded)
			require.NoError(t, err)
			require.Equal(t, hrp, gotHRP)
			require.Equal(t, testData, gotData)
		})
	}
}

// TestBech32DecodeErrors verifies that various malformed inputs produce errors.
func TestBech32DecodeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "no separator",
			input: "lnoabcdef",
		},
		{
			name:  "separator only",
			input: "1",
		},
		{
			name:  "no data after separator",
			input: "lno1",
		},
		{
			name:  "invalid character",
			input: "lno1b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := Decode(tc.input)
			require.Error(t, err)
		})
	}
}

// TestStripContinuation verifies the '+' stripping logic in isolation.
func TestStripContinuation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "no continuation",
			input: "lno1acd",
			want:  "lno1acd",
		},
		{
			name:  "simple continuation",
			input: "lno1a+cd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with whitespace",
			input: "lno1a+ cd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with newline",
			input: "lno1a+\ncd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with crlf and space",
			input: "lno1a+\r\n cd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with vertical tab",
			input: "lno1a+\vcd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with form feed",
			input: "lno1a+\fcd",
			want:  "lno1acd",
		},
		{
			name:  "continuation with every ascii whitespace",
			input: "lno1a+ \t\n\v\f\rcd",
			want:  "lno1acd",
		},
		{
			name:    "trailing plus",
			input:   "lno1acd+",
			wantErr: true,
		},
		{
			name:    "trailing plus with space",
			input:   "lno1acd+ ",
			wantErr: true,
		},
		{
			name:    "leading plus",
			input:   "+lno1acd",
			wantErr: true,
		},
		{
			name:    "leading plus with whitespace",
			input:   "\n+lno1acd",
			wantErr: true,
		},
		{
			name:    "consecutive plus",
			input:   "lno1a++cd",
			wantErr: true,
		},
		{
			name:    "plus joined to plus by whitespace",
			input:   "lno1a+ +cd",
			wantErr: true,
		},
		{
			name:  "plus inside the prefix",
			input: "ln+o1pqps7sjq",
			want:  "lno1pqps7sjq",
		},
		{
			name:  "plus before the separator",
			input: "lno+1pqps7sjq",
			want:  "lno1pqps7sjq",
		},
		{
			name:  "plus after the separator",
			input: "lno1+pqps7sjq",
			want:  "lno1pqps7sjq",
		},
		{
			name:  "plus inside the prefix with whitespace",
			input: "ln+\r\n o1pqps7sjq",
			want:  "lno1pqps7sjq",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := stripContinuation(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestDecodeContinuationAnywhere asserts that a marker at each interior
// position keeps the decoded data the same. The positions include the
// prefix and both sides of the '1' separator. The spec requires removal only
// between two bech32 characters. A writer, however, wraps a line where the
// medium makes it necessary, and the other implementations join anywhere. A
// marker must therefore never change the meaning of a string.
func TestDecodeContinuationAnywhere(t *testing.T) {
	t.Parallel()

	payload := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	encoded, err := Encode(HRPOffer, payload)
	require.NoError(t, err)

	for i := 1; i < len(encoded); i++ {
		split := encoded[:i] + "+" + encoded[i:]

		hrp, data, err := Decode(split)
		require.NoError(t, err, "marker at position %d", i)
		require.Equal(t, HRPOffer, hrp)
		require.Equal(t, payload, data)
	}
}

// TestEncodeUnknownHRP asserts that Encode takes only the prefixes in
// validHRPs, so a caller cannot make a string that Decode refuses. The message
// must also name each accepted prefix, because the message and the membership
// test read one list.
func TestEncodeUnknownHRP(t *testing.T) {
	t.Parallel()

	_, err := Encode("bogus", []byte{0x00})
	require.ErrorIs(t, err, ErrUnsupportedHRP)

	for _, hrp := range validHRPs {
		require.Contains(t, err.Error(), hrp)
	}
}

// TestDecodeUnknownHRP asserts that Decode rejects strings with unsupported
// HRPs.
func TestDecodeUnknownHRP(t *testing.T) {
	t.Parallel()

	_, _, err := Decode("bogus1pqps7sjq")
	require.ErrorIs(t, err, ErrUnsupportedHRP)
}

// TestDecodeUnprintableCharacter asserts that Decode rejects characters outside
// printable ASCII range (33..126).
func TestDecodeUnprintableCharacter(t *testing.T) {
	t.Parallel()

	// The last three characters are whitespace. A string can hold
	// whitespace only after a '+' marker. In each other position it is a
	// byte below the printable range.
	unprintable := []string{
		"l\x1b[31mno1pqps7sjq",
		"l\x00no1pqps7sjq",
		"ln\no1pqps7sjq",
		"ln\vo1pqps7sjq",
		"ln\fo1pqps7sjq",
	}

	for _, input := range unprintable {
		_, _, err := Decode(input)
		require.ErrorIs(t, err, ErrInvalidCharacter)
	}
}

// TestDecodeOversizeInput asserts the input length cap fires before any
// allocation.
func TestDecodeOversizeInput(t *testing.T) {
	t.Parallel()

	// A raw string above the transport limit is rejected.
	huge := strings.Repeat("a", maxBolt12RawStringLen+1)
	_, _, err := Decode(huge)
	require.ErrorIs(t, err, ErrStringTooLong)

	// A string under the raw limit but over the cleaned limit is
	// rejected after stripping.
	oversize := strings.Repeat("a", maxBolt12StringLen+1)
	_, _, err = Decode(oversize)
	require.ErrorIs(t, err, ErrStringTooLong)

	// A string at the cleaned limit is accepted, but here leads to a
	// parsing error.
	oversize = strings.Repeat("a", maxBolt12StringLen)
	_, _, err = Decode(oversize)
	require.ErrorIs(t, err, ErrInvalidSeparator)
}

// TestDecodeWrappedMaxPayload asserts that a legal continuation wrapping of the
// longest string Encode can make still decodes. The cleaned limit governs the
// payload, and the raw limit leaves room for the wrapping.
func TestDecodeWrappedMaxPayload(t *testing.T) {
	t.Parallel()

	payload := make([]byte, maxBolt12DataLen)
	encoded, err := Encode(HRPOffer, payload)
	require.NoError(t, err)
	require.Len(t, encoded, maxBolt12StringLen)

	// Insert a marker and a whitespace run into the data part. The raw
	// string grows past the cleaned limit but stays under the raw one.
	wrapped := encoded[:100] + "+ \n\t" + encoded[100:]
	require.Greater(t, len(wrapped), maxBolt12StringLen)

	hrp, data, err := Decode(wrapped)
	require.NoError(t, err)
	require.Equal(t, HRPOffer, hrp)
	require.Equal(t, payload, data)
}

// TestHRPLenMatchesBudget asserts the fixed prefix cost that the character
// limit assumes. A prefix longer than bolt12HRPLen would let Encode make one
// more character than Decode accepts. The shared limit exists to prevent this
// difference.
func TestHRPLenMatchesBudget(t *testing.T) {
	t.Parallel()

	for _, hrp := range validHRPs {
		require.Len(t, hrp, bolt12HRPLen)
	}
}

// TestEncodePayloadSize asserts which payload sizes Encode takes and which it
// rejects. The rows walk the size axis from below the shortest legal payload to
// above the longest, and each accepted row decodes back to its input. The table
// therefore holds both ends of the size contract in one place.
func TestEncodePayloadSize(t *testing.T) {
	t.Parallel()

	// maxOfferFields is the payload of an offer that holds a metadata
	// field, a description field, and an issuer field, each at the largest
	// record the decoder takes.
	const maxOfferFields = 3 * (1 + 3 + math.MaxUint16)

	tests := []struct {
		name    string
		payload []byte
		wantErr error

		// wantLen, when set, is the exact length of the string that
		// Encode must make.
		wantLen int
	}{
		{
			name:    "nil payload",
			payload: nil,
			wantErr: ErrEmptyString,
		},
		{
			name:    "empty payload",
			payload: []byte{},
			wantErr: ErrEmptyString,
		},
		{
			name:    "one byte",
			payload: make([]byte, 1),
		},
		{
			name:    "three maximal offer fields",
			payload: make([]byte, maxOfferFields),
		},
		{
			name:    "longest payload",
			payload: make([]byte, maxBolt12DataLen),
			wantLen: maxBolt12StringLen,
		},
		{
			name:    "one byte above the longest payload",
			payload: make([]byte, maxBolt12DataLen+1),
			wantErr: ErrStringTooLong,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := Encode(HRPOffer, tc.payload)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			if tc.wantLen != 0 {
				require.Len(t, encoded, tc.wantLen)
			}

			// Decode takes each string that Encode makes.
			hrp, data, err := Decode(encoded)
			require.NoError(t, err)
			require.Equal(t, HRPOffer, hrp)
			require.Equal(t, tc.payload, data)
		})
	}
}

// TestDecodeUppercase pins the spec MUST that readers handle both all-lowercase
// and all-uppercase strings: a payload encoded lowercase, then ToUpper'd in
// transit (e.g. QR code), must decode back to the same HRP and bytes.
func TestDecodeUppercase(t *testing.T) {
	t.Parallel()

	payload := []byte{0x01, 0x23, 0x45, 0x67}
	encoded, err := Encode(HRPOffer, payload)
	require.NoError(t, err)

	uppered := strings.ToUpper(encoded)
	require.NotEqual(t, encoded, uppered)

	hrp, data, err := Decode(uppered)
	require.NoError(t, err)
	require.Equal(t, HRPOffer, hrp)
	require.Equal(t, payload, data)
}

// TestPropertyBech32RoundTrip asserts Encode and Decode form a bijection for
// arbitrary data payloads under each of the three BOLT 12 HRPs. The codec's
// correctness depends on this property. A hand-rolled table can only hit a
// small number of payload sizes, while rapid drives shrinking generators across
// the whole input space and minimizes any counter-example it finds.
func TestPropertyBech32RoundTrip(t *testing.T) {
	t.Parallel()

	hrps := []string{HRPOffer, HRPInvoiceRequest, HRPInvoice}

	rapid.Check(t, func(t *rapid.T) {
		hrp := hrps[rapid.IntRange(0, len(hrps)-1).Draw(t, "hrp")]
		// Draw the payload from the range that both ends of the
		// codec accept, because the bijection holds in that range.
		// The upper bound here stays far below the limit, so rapid
		// works on the content of the payload and not on its
		// length.
		size := rapid.IntRange(1, 1024).Draw(t, "size")
		data := rapid.SliceOfN(
			rapid.Byte(), size, size,
		).Draw(t, "data")

		encoded, err := Encode(hrp, data)
		require.NoError(t, err)

		decodedHRP, decodedData, err := Decode(encoded)
		require.NoError(t, err)
		require.Equal(t, hrp, decodedHRP)
		require.Equal(t, data, decodedData)
	})
}
