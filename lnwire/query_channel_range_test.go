package lnwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueryChannelRange tests that a few query_channel_range test vectors can
// correctly be decoded and encoded.
func TestQueryChannelRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		input             string
		expFirstBlockNum  int
		expNumOfBlocks    int
		expWantTimestamps bool
	}{
		{
			name: "without timestamps query option",
			input: "01070f9188f13cb7b2c71f2a335e3a4fc328bf5beb436" +
				"012afca590b1a11466e2206000186a0000005dc",
			expFirstBlockNum:  100000,
			expNumOfBlocks:    1500,
			expWantTimestamps: false,
		},
		{
			name: "with timestamps query option",
			input: "01070f9188f13cb7b2c71f2a335e3a4fc328bf5beb436" +
				"012afca590b1a11466e2206000088b800000064010103",
			expFirstBlockNum:  35000,
			expNumOfBlocks:    100,
			expWantTimestamps: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			b, err := hex.DecodeString(test.input)
			require.NoError(t, err)

			r := bytes.NewBuffer(b)

			msg, err := ReadMessage(r, 0)
			require.NoError(t, err)

			queryMsg, ok := msg.(*QueryChannelRange)
			require.True(t, ok)

			require.EqualValues(
				t, test.expFirstBlockNum,
				queryMsg.FirstBlockHeight,
			)

			require.EqualValues(
				t, test.expNumOfBlocks, queryMsg.NumBlocks,
			)

			require.Equal(
				t, test.expWantTimestamps,
				queryMsg.WithTimestamps(),
			)

			var buf bytes.Buffer
			_, err = WriteMessage(&buf, queryMsg, 0)
			require.NoError(t, err)

			require.Equal(t, buf.Bytes(), b)
		})
	}
}
