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
				t.Fatalf("expected ErrUnsortedSIDs, got: %T",
					err)
			}
		})
	}
}

// TestReplyChannelRangeEmpty tests encoding and decoding a ReplyChannelRange
// that doesn't contain any channel results.
func TestReplyChannelRangeEmpty(t *testing.T) {
	emptyChannelsTests := []struct {
		name       string
		encType    ShortChanIDEncoding
		encodedHex string
	}{
		{
			name:       "empty plain encoding",
			encType:    EncodingSortedPlain,
			encodedHex: "0000000000000000000000000000000000000000000000000000000000000000000000010000000201000100",
		},
		{
			name:       "empty zlib encoding",
			encType:    EncodingSortedZlib,
			encodedHex: "0000000000000000000000000000000000000000000000000000000000000000000000010000000201000101",
		},
	}

	for _, test := range emptyChannelsTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			req := ReplyChannelRange{
				QueryChannelRange: QueryChannelRange{
					FirstBlockHeight: 1,
					NumBlocks:        2,
				},
				Complete:     1,
				EncodingType: test.encType,
				ShortChanIDs: nil,
			}

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
