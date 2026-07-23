package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

type unsortedSidTest struct {
	name    string
	encType QueryEncoding
	sids    []ShortChannelID
}

var (
	unsortedSids = []ShortChannelID{
		NewShortChanIDFromInt(4),
		NewShortChanIDFromInt(3),
	}

	duplicateSids = []ShortChannelID{
		NewShortChanIDFromInt(3),
		NewShortChanIDFromInt(3),
	}

	unsortedSidTests = []unsortedSidTest{
		{
			name:    "plain unsorted",
			encType: EncodingSortedPlain,
			sids:    unsortedSids,
		},
		{
			name:    "plain duplicate",
			encType: EncodingSortedPlain,
			sids:    duplicateSids,
		},
		{
			name:    "zlib unsorted",
			encType: EncodingSortedZlib,
			sids:    unsortedSids,
		},
		{
			name:    "zlib duplicate",
			encType: EncodingSortedZlib,
			sids:    duplicateSids,
		},
	}
)

// TestQueryShortChanIDsUnsorted tests that decoding a QueryShortChanID request
// that contains duplicate or unsorted ids returns an ErrUnsortedSIDs failure.
func TestQueryShortChanIDsUnsorted(t *testing.T) {
	for _, test := range unsortedSidTests {
		t.Run(test.name, func(t *testing.T) {
			req := &QueryShortChanIDs{
				EncodingType: test.encType,
				ShortChanIDs: test.sids,
				noSort:       true,
			}

			var b bytes.Buffer
			err := req.Encode(&b, 0)
			if err != nil {
				t.Fatalf("unable to encode req: %v", err)
			}

			var req2 QueryShortChanIDs
			err = req2.Decode(bytes.NewReader(b.Bytes()), 0)
			if _, ok := err.(ErrUnsortedSIDs); !ok {
				t.Fatalf("expected ErrUnsortedSIDs, got: %T",
					err)
			}
		})
	}
}

// TestQueryShortChanIDsZero ensures that decoding of a list of short chan ids
// still works as expected when the first element of the list is zero.
func TestQueryShortChanIDsZero(t *testing.T) {
	testCases := []struct {
		name     string
		encoding QueryEncoding
	}{
		{
			name:     "plain",
			encoding: EncodingSortedPlain,
		}, {
			name:     "zlib",
			encoding: EncodingSortedZlib,
		},
	}

	testSids := []ShortChannelID{
		NewShortChanIDFromInt(0),
		NewShortChanIDFromInt(10),
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			req := &QueryShortChanIDs{
				EncodingType: test.encoding,
				ShortChanIDs: testSids,
				noSort:       true,
			}

			var b bytes.Buffer
			err := req.Encode(&b, 0)
			if err != nil {
				t.Fatalf("unable to encode req: %v", err)
			}

			var req2 QueryShortChanIDs
			err = req2.Decode(bytes.NewReader(b.Bytes()), 0)
			if err != nil {
				t.Fatalf("unexpected decoding error: %v", err)
			}
		})
	}
}

// TestQueryShortChanIDsRoundTrip uses property-based testing to ensure both
// supported encodings preserve sorted short channel ID sets.
func TestQueryShortChanIDsRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		encoding := rapid.SampledFrom([]QueryEncoding{
			EncodingSortedPlain,
			EncodingSortedZlib,
		}).Draw(t, "encoding")

		numSCIDs := rapid.IntRange(0, 512).Draw(t, "num-scids")
		var scids []ShortChannelID
		if numSCIDs > 0 {
			scids = make([]ShortChannelID, numSCIDs)
		}

		offset := rapid.IntRange(0, 1_000_000).Draw(t, "offset")
		step := rapid.IntRange(1, 1_000_000).Draw(t, "step")
		for i := range scids {
			scid := uint64(offset + i*step)
			scids[i] = NewShortChanIDFromInt(scid)
		}

		var b bytes.Buffer
		require.NoError(t, encodeShortChanIDs(
			&b, encoding, scids,
		))

		decodedEncoding, decoded, err := decodeShortChanIDs(
			bytes.NewReader(b.Bytes()),
		)
		require.NoError(t, err)
		require.Equal(t, encoding, decodedEncoding)
		require.Equal(t, scids, decoded)
	})
}

// TestQueryShortChanIDsDecodeLimit ensures that a decompressed short channel
// ID stream cannot exceed its resource limit.
func TestQueryShortChanIDsDecodeLimit(t *testing.T) {
	t.Parallel()

	var stream bytes.Buffer
	for i := 0; i <= maxDecodedShortChanIDs; i++ {
		require.NoError(t, WriteElements(
			&stream, NewShortChanIDFromInt(uint64(i)),
		))
	}

	decoded, err := decodeCompressedShortChanIDs(bytes.NewReader(
		stream.Bytes()[:maxDecodedShortChanIDs*8],
	))
	require.NoError(t, err)
	require.Len(t, decoded, maxDecodedShortChanIDs)

	_, err = decodeCompressedShortChanIDs(
		bytes.NewReader(stream.Bytes()),
	)
	require.ErrorContains(t, err, "too many short channel IDs")
}

// TestQueryShortChanIDsZlibCompatibility ensures that a protocol-valid
// compressed reply can contain more short channel IDs than a plain reply.
func TestQueryShortChanIDsZlibCompatibility(t *testing.T) {
	t.Parallel()

	const numSCIDs = 30_795

	scids := make([]ShortChannelID, numSCIDs)
	for i := range scids {
		scids[i] = NewShortChanIDFromInt(uint64(i))
	}

	var b bytes.Buffer
	require.NoError(t, encodeShortChanIDs(
		&b, EncodingSortedZlib, scids,
	))

	encoding, decoded, err := decodeShortChanIDs(
		bytes.NewReader(b.Bytes()),
	)
	require.NoError(t, err)
	require.Equal(t, EncodingSortedZlib, encoding)
	require.Equal(t, scids, decoded)
}

// TestQueryShortChanIDsRejectsCorruptZlib ensures that truncated or corrupt
// compressed streams are not accepted as valid partial replies.
func TestQueryShortChanIDsRejectsCorruptZlib(t *testing.T) {
	t.Parallel()

	scids := []ShortChannelID{
		NewShortChanIDFromInt(1),
		NewShortChanIDFromInt(2),
		NewShortChanIDFromInt(3),
	}

	var encoded bytes.Buffer
	require.NoError(t, encodeShortChanIDs(
		&encoded, EncodingSortedZlib, scids,
	))

	body := encoded.Bytes()[2:]
	corruptChecksum := append([]byte(nil), body...)
	corruptChecksum[len(corruptChecksum)-1] ^= 1

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "truncated header",
			body: body[:2],
		},
		{
			name: "truncated checksum",
			body: body[:len(body)-1],
		},
		{
			name: "corrupt checksum",
			body: corruptChecksum,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var message bytes.Buffer
			require.NoError(t, WriteElements(
				&message, uint16(len(test.body)),
			))
			_, err := message.Write(test.body)
			require.NoError(t, err)

			_, _, err = decodeShortChanIDs(
				bytes.NewReader(message.Bytes()),
			)
			require.Error(t, err)
		})
	}
}
