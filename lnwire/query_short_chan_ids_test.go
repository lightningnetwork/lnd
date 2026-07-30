package lnwire

import (
	"bytes"
	"encoding/hex"
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

// TestQueryShortChanIDsRoundTrip uses property-based testing to ensure plain
// encoding preserves sorted short channel ID sets.
func TestQueryShortChanIDsRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
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
			&b, EncodingSortedPlain, scids,
		))

		decodedEncoding, decoded, err := decodeShortChanIDs(
			bytes.NewReader(b.Bytes()),
		)
		require.NoError(t, err)
		require.Equal(t, EncodingSortedPlain, decodedEncoding)
		require.Equal(t, scids, decoded)
	})
}

// TestEncodeShortChanIDsZlibRejection tests that attempting to encode using
// the deprecated zlib encoding format returns an ErrZlibNotSupported failure.
func TestEncodeShortChanIDsZlibRejection(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer

	err := encodeShortChanIDs(&b, EncodingSortedZlib, nil)

	require.ErrorIs(t, err, ErrZlibNotSupported)
}

// TestDecodeShortChanIDsZlibRejection tests that decoding a query that uses
// the deprecated zlib encoding returns ErrZlibNotSupported.
func TestDecodeShortChanIDsZlibRejection(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	buf.Write(make([]byte, 32))
	buf.Write([]byte{0x00, 0x16})
	buf.WriteByte(byte(EncodingSortedZlib))
	payload, err := hex.DecodeString(
		"789c636000833e08659309a65c971d0100126e02e3",
	)
	require.NoError(t, err)
	buf.Write(payload)

	var q QueryShortChanIDs
	err = q.Decode(bytes.NewReader(buf.Bytes()), 0)
	require.ErrorIs(t, err, ErrZlibNotSupported)
}
