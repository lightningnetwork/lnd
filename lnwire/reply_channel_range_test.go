package lnwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
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
		encType    QueryEncoding
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
				ExtraData:        make([]byte, 0),
			}

			// First decode the hex string in the test case into a
			// new ReplyChannelRange message. It should be
			// identical to the one created above.
			req2 := NewReplyChannelRange()
			b, _ := hex.DecodeString(test.encodedHex)
			err := req2.Decode(bytes.NewReader(b), 0)
			require.NoError(t, err)
			require.Equal(t, req, *req2)

			// Next, we go in the reverse direction: encode the
			// request created above, and assert that it matches
			// the raw byte encoding.
			var b2 bytes.Buffer
			err = req.Encode(&b2, 0)
			require.NoError(t, err)
			require.Equal(t, b, b2.Bytes())
		})
	}
}

// TestReplyChannelRangeEncode tests that encoding a ReplyChannelRange message
// results in the correct sorting of the SCIDs and Timestamps.
func TestReplyChannelRangeEncode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		scids         []ShortChannelID
		timestamps    Timestamps
		expError      string
		expScids      []ShortChannelID
		expTimestamps Timestamps
	}{
		{
			name: "scids only, sorted",
			scids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
			expScids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
		},
		{
			name: "scids only, unsorted",
			scids: []ShortChannelID{
				{BlockHeight: 300},
				{BlockHeight: 100},
				{BlockHeight: 200},
			},
			expScids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
		},
		{
			name: "scids and timestamps, sorted",
			scids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
			timestamps: Timestamps{
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
				{Timestamp1: 5, Timestamp2: 6},
			},
			expScids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
			expTimestamps: Timestamps{
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
				{Timestamp1: 5, Timestamp2: 6},
			},
		},
		{
			name: "scids and timestamps, unsorted",
			scids: []ShortChannelID{
				{BlockHeight: 300},
				{BlockHeight: 100},
				{BlockHeight: 200},
			},
			timestamps: Timestamps{
				{Timestamp1: 5, Timestamp2: 6},
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
			},
			expScids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
			expTimestamps: Timestamps{
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
				{Timestamp1: 5, Timestamp2: 6},
			},
		},
		{
			name: "scid and timestamp count does not match",
			scids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 300},
			},
			timestamps: Timestamps{
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
			},
			expError: "got must provide a timestamp pair for " +
				"each of the given SCIDs",
		},
		{
			name: "duplicate scids",
			scids: []ShortChannelID{
				{BlockHeight: 100},
				{BlockHeight: 200},
				{BlockHeight: 200},
			},
			timestamps: Timestamps{
				{Timestamp1: 1, Timestamp2: 2},
				{Timestamp1: 3, Timestamp2: 4},
				{Timestamp1: 5, Timestamp2: 6},
			},
			expError: "scid list should not contain duplicates",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			replyMsg := &ReplyChannelRange{
				FirstBlockHeight: 1,
				NumBlocks:        2,
				Complete:         1,
				EncodingType:     EncodingSortedPlain,
				ShortChanIDs:     test.scids,
				Timestamps:       test.timestamps,
				ExtraData:        make([]byte, 0),
			}

			var buf bytes.Buffer
			_, err := WriteMessage(&buf, replyMsg, 0)
			if len(test.expError) != 0 {
				require.ErrorContains(t, err, test.expError)

				return
			}

			require.NoError(t, err)

			r := bytes.NewBuffer(buf.Bytes())
			msg, err := ReadMessage(r, 0)
			require.NoError(t, err)

			msg2, ok := msg.(*ReplyChannelRange)
			require.True(t, ok)

			require.Equal(t, test.expScids, msg2.ShortChanIDs)
			require.Equal(t, test.expTimestamps, msg2.Timestamps)
		})
	}
}

// TestReplyChannelRangeDecode tests the decoding of some ReplyChannelRange
// test vectors.
func TestReplyChannelRangeDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		hex           string
		expEncoding   QueryEncoding
		expSCIDs      []string
		expTimestamps Timestamps
		expError      string
	}{
		{
			name: "plain encoding",
			hex: "01080f9188f13cb7b2c71f2a335e3a4fc328bf5beb4360" +
				"12afca590b1a11466e2206000b8a06000005dc01001" +
				"900000000000000008e0000000000003c6900000000" +
				"0045a6c4",
			expEncoding: EncodingSortedPlain,
			expSCIDs: []string{
				"0:0:142",
				"0:0:15465",
				"0:69:42692",
			},
		},
		{
			name: "zlib encoding",
			hex: "01080f9188f13cb7b2c71f2a335e3a4fc328bf5beb4360" +
				"12afca590b1a11466e2206000006400000006e010016" +
				"01789c636000833e08659309a65878be010010a9023a",
			expEncoding: EncodingSortedZlib,
			expSCIDs: []string{
				"0:0:142",
				"0:0:15465",
				"0:4:3318",
			},
		},
		{
			name: "plain encoding including timestamps",
			hex: "01080f9188f13cb7b2c71f2a335e3a4fc328bf5beb43601" +
				"2afca590b1a11466e22060001ddde000005dc0100190" +
				"0000000000000304300000000000778d600000000004" +
				"6e1c1011900000282c1000e77c5000778ad00490ab00" +
				"000b57800955bff031800000457000008ae00000d050" +
				"000115c000015b300001a0a",
			expEncoding: EncodingSortedPlain,
			expSCIDs: []string{
				"0:0:12355",
				"0:7:30934",
				"0:70:57793",
			},
			expTimestamps: Timestamps{
				{
					Timestamp1: 164545,
					Timestamp2: 948165,
				},
				{
					Timestamp1: 489645,
					Timestamp2: 4786864,
				},
				{
					Timestamp1: 46456,
					Timestamp2: 9788415,
				},
			},
		},
		{
			name: "unsupported encoding",
			hex: "01080f9188f13cb7b2c71f2a335e3a4fc328bf5beb" +
				"436012afca590b1a11466e22060001ddde000005dc01" +
				"001801789c63600001036730c55e710d4cbb3d3c0800" +
				"17c303b1012201789c63606a3ac8c0577e9481bd622d" +
				"8327d7060686ad150c53a3ff0300554707db03180000" +
				"0457000008ae00000d050000115c000015b300001a0a",
			expError: "unsupported encoding",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			b, err := hex.DecodeString(test.hex)
			require.NoError(t, err)

			r := bytes.NewBuffer(b)

			msg, err := ReadMessage(r, 0)
			if len(test.expError) != 0 {
				require.ErrorContains(t, err, test.expError)

				return
			}
			require.NoError(t, err)

			replyMsg, ok := msg.(*ReplyChannelRange)
			require.True(t, ok)
			require.Equal(
				t, test.expEncoding, replyMsg.EncodingType,
			)

			scids := make([]string, len(replyMsg.ShortChanIDs))
			for i, id := range replyMsg.ShortChanIDs {
				scids[i] = id.String()
			}
			require.Equal(t, scids, test.expSCIDs)

			require.Equal(
				t, test.expTimestamps, replyMsg.Timestamps,
			)
		})
	}
}
