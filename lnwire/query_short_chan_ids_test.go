package lnwire

import (
	"bytes"
	"testing"
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
		test := test
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
		test := test
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
