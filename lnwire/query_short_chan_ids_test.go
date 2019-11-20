package lnwire

import (
	"bytes"
	"testing"
)

type unsortedSidTest struct {
	name    string
	encType ShortChanIDEncoding
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
