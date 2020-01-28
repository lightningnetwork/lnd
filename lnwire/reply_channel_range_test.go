package lnwire

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

// TestReplyChannelRangeUnsorted tests that decoding a ReplyChannelRange request
// that contains duplicate or unsorted ids returns an ErrUnsortedSIDs failure.
func TestReplyChannelRangeUnsorted(t *testing.T) {
	for _, test := range unsortedSidTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			req := &ReplyChannelRange{
				EncodingType: test.encType,
				ShortChanIDs: test.sids,
				noSort:       true,
			}

			var b bytes.Buffer
			err := req.Encode(&b, 0)
			if err != nil {
				t.Fatalf("unable to encode req: %v", err)
			}

			var req2 ReplyChannelRange
			err = req2.Decode(bytes.NewReader(b.Bytes()), 0)
			if _, ok := err.(ErrUnsortedSIDs); !ok {
				t.Fatalf("expected ErrUnsortedSIDs, got: %v",
					err)
			}
		})
	}
}

// TestReplyChannelRangeEmpty tests encoding and decoding a ReplyChannelRange
// that doesn't contain any channel results.
func TestReplyChannelRangeEmpty(t *testing.T) {
	t.Parallel()

	emptyChannelsTests := []struct {
		name       string
		encType    ShortChanIDEncoding
		encodedHex string
	}{
		{
			name:    "empty plain encoding",
			encType: EncodingSortedPlain,
			encodedHex: "000000000000000000000000000000000000000" +
				"00000000000000000000000000000000100000002" +
				"01000100",
		},
		{
			name:    "empty zlib encoding",
			encType: EncodingSortedZlib,
			encodedHex: "00000000000000000000000000000000000000" +
				"0000000000000000000000000000000001000000" +
				"0201000101",
		},
	}

	for _, test := range emptyChannelsTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			req := ReplyChannelRange{
				FirstBlockHeight: 1,
				NumBlocks:        2,
				Complete:         1,
				EncodingType:     test.encType,
				ShortChanIDs:     nil,
			}

			// First decode the hex string in the test case into a
			// new ReplyChannelRange message. It should be
			// identical to the one created above.
			var req2 ReplyChannelRange
			b, _ := hex.DecodeString(test.encodedHex)
			err := req2.Decode(bytes.NewReader(b), 0)
			if err != nil {
				t.Fatalf("unable to decode req: %v", err)
			}
			if !reflect.DeepEqual(req, req2) {
				t.Fatalf("requests don't match: expected %v got %v",
					spew.Sdump(req), spew.Sdump(req2))
			}

			// Next, we go in the reverse direction: encode the
			// request created above, and assert that it matches
			// the raw byte encoding.
			var b2 bytes.Buffer
			err = req.Encode(&b2, 0)
			if err != nil {
				t.Fatalf("unable to encode req: %v", err)
			}
			if !bytes.Equal(b, b2.Bytes()) {
				t.Fatalf("encoded requests don't match: expected %x got %x",
					b, b2.Bytes())
			}
		})
	}
}
