package commands

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseChanPoint tests parseChanPoint with various
// valid and invalid input values and verifies the output.
func TestParseChanPoint(t *testing.T) {
	testCases := []struct {
		channelPoinStr    string
		channelPointIsNil bool
		outputIndex       uint32
		err               error
	}{
		{
			"24581424081379576b4a7580ace91db10925d996a2a8d45c8034" +
				"3a5a467dc0bc:0",
			false,
			0,
			nil,
		}, {
			"24581424081379576b4a7580ace91db10925d996a2a8d45c8034" +
				"3a5a467dc0bc:4",
			false,
			4,
			nil,
		}, {
			":",
			true,
			0,
			errBadChanPoint,
		}, {
			":0",
			true,
			0,
			errBadChanPoint,
		}, {
			"24581424081379576b4a7580ace91db10925d996a2a8d45c8034" +
				"3a5a467dc0bc:",
			true,
			0,
			errBadChanPoint,
		}, {
			"24581424081379576b4a7580ace91db10925d996a2a8d45c8034" +
				"3a5a467dc0bc:string",
			true,
			0,
			strconv.ErrSyntax,
		}, {
			"not_hex:0",
			true,
			0,
			hex.InvalidByteError('n'),
		},
	}
	for _, tc := range testCases {
		cp, err := parseChanPoint(tc.channelPoinStr)
		require.ErrorIs(t, err, tc.err)

		require.Equal(t, tc.channelPointIsNil, cp == nil)
		if !tc.channelPointIsNil {
			require.Equal(t, tc.outputIndex, cp.OutputIndex)
		}
	}
}

// TestParseTimeLockDelta tests parseTimeLockDelta with various
// valid and invalid input values and verifies the output.
func TestParseTimeLockDelta(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		timeLockDeltaStr      string
		expectedTimeLockDelta uint16
		expectErr             bool
		expectedErrContent    string
	}{
		{
			timeLockDeltaStr: "-1",
			expectErr:        true,
			expectedErrContent: fmt.Sprintf(
				"failed to parse time_lock_delta: %d "+
					"to uint64", -1,
			),
		},
		{
			timeLockDeltaStr: "0",
		},
		{
			timeLockDeltaStr:      "3",
			expectedTimeLockDelta: 3,
		},
		{
			timeLockDeltaStr: strconv.FormatUint(
				uint64(math.MaxUint16), 10,
			),
			expectedTimeLockDelta: math.MaxUint16,
		},
		{
			timeLockDeltaStr: "18446744073709551616",
			expectErr:        true,
			expectedErrContent: fmt.Sprint(
				"failed to parse time_lock_delta:" +
					" 18446744073709551616 to uint64"),
		},
	}
	for _, tc := range testCases {
		timeLockDelta, err := parseTimeLockDelta(tc.timeLockDeltaStr)
		require.Equal(t, tc.expectedTimeLockDelta, timeLockDelta)
		if tc.expectErr {
			require.ErrorContains(t, err, tc.expectedErrContent)
		} else {
			require.NoError(t, err)
		}
	}
}

// TestReplaceCustomData tests that hex encoded custom data can be formatted as
// JSON in the console output.
func TestReplaceCustomData(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "no replacement necessary",
			data:     "foo",
			expected: "foo",
		},
		{
			name: "valid json with replacement",
			data: `{"foo":"bar","custom_channel_data":"` +
				hex.EncodeToString([]byte(
					`{"bar":"baz"}`,
				)) + `"}`,
			expected: `{
    "foo": "bar",
    "custom_channel_data": {
        "bar": "baz"
    }
}`,
		},
		{
			name: "valid json with replacement and space",
			data: `{"foo":"bar","custom_channel_data": "` +
				hex.EncodeToString([]byte(
					`{"bar":"baz"}`,
				)) + `"}`,
			expected: `{
    "foo": "bar",
    "custom_channel_data": {
        "bar": "baz"
    }
}`,
		},
		{
			name: "doesn't match pattern, returned identical",
			data: "this ain't even json, and no custom data " +
				"either",
			expected: "this ain't even json, and no custom data " +
				"either",
		},
		{
			name: "invalid json",
			data: "this ain't json, " +
				"\"custom_channel_data\":\"a\"",
			expected: "this ain't json, " +
				"\"custom_channel_data\":\"a\"",
		},
		{
			name: "valid json, invalid hex, just formatted",
			data: `{"custom_channel_data":"f"}`,
			expected: `{
    "custom_channel_data": "f"
}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := replaceCustomData([]byte(tc.data))
			require.Equal(t, tc.expected, string(result))
		})
	}
}

// TestReplaceAndAppendScid tests whether chan_id is replaced with scid and
// scid_str in the JSON console output.
func TestReplaceAndAppendScid(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "no replacement necessary",
			data:     "foo",
			expected: "foo",
		},
		{
			name: "valid json with replacement",
			data: `{"foo":"bar","chan_id":"829031767408640"}`,
			expected: `{
    "foo": "bar",
    "scid": "829031767408640",
    "scid_str": "754x1x0"
}`,
		},
		{
			name: "valid json with replacement and space",
			data: `{"foo":"bar","chan_id": "829031767408640"}`,
			expected: `{
    "foo": "bar",
    "scid": "829031767408640",
    "scid_str": "754x1x0"
}`,
		},
		{
			name: "doesn't match pattern, returned identical",
			data: "this ain't even json, and no chan_id " +
				"either",
			expected: "this ain't even json, and no chan_id " +
				"either",
		},
		{
			name: "invalid json",
			data: "this ain't json, " +
				"\"chan_id\":\"18446744073709551616\"",
			expected: "this ain't json, " +
				"\"chan_id\":\"18446744073709551616\"",
		},
		{
			name: "valid json, invalid uint, just formatted",
			data: `{"chan_id":"18446744073709551616"}`,
			expected: `{
    "chan_id": "18446744073709551616"
}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := replaceAndAppendScid([]byte(tc.data))
			require.Equal(t, tc.expected, string(result))
		})
	}
}

// TestAppendChanID tests whether chan_id (BOLT02) is appended
// to the JSON console output.
func TestAppendChanID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "no amendment necessary",
			data:     "foo",
			expected: "foo",
		},
		{
			name: "valid json with amendment",
			data: `{"foo":"bar","channel_point":"6ab312e3b744e` +
				`1b80a33a6541697df88766515c31c08e839bf11dc` +
				`9fcc036a19:0"}`,
			expected: `{
    "foo": "bar",
    "channel_point": "6ab312e3b744e1b80a33a6541697df88766515c31c` +
				`08e839bf11dc9fcc036a19:0",
    "chan_id": "196a03cc9fdc11bf39e8081cc315657688df971654a` +
				`6330ab8e144b7e312b36a"
}`,
		},
		{
			name: "valid json with amendment and space",
			data: `{"foo":"bar","channel_point": "6ab312e3b744e` +
				`1b80a33a6541697df88766515c31c08e839bf11dc` +
				`9fcc036a19:0"}`,
			expected: `{
    "foo": "bar",
    "channel_point": "6ab312e3b744e1b80a33a6541697df88766515c31c` +
				`08e839bf11dc9fcc036a19:0",
    "chan_id": "196a03cc9fdc11bf39e8081cc315657688df971654a` +
				`6330ab8e144b7e312b36a"
}`,
		},
		{
			name: "doesn't match pattern, returned identical",
			data: "this ain't even json, and no channel_point " +
				"either",
			expected: "this ain't even json, and no channel_point" +
				" either",
		},
		{
			name: "invalid json",
			data: "this ain't json, " +
				"\"channel_point\":\"f:0\"",
			expected: "this ain't json, " +
				"\"channel_point\":\"f:0\"",
		},
		{
			name: "valid json with invalid outpoint, formatted",
			data: `{"channel_point":"f:0"}`,
			expected: `{
    "channel_point": "f:0"
}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := appendChanID([]byte(tc.data))
			require.Equal(t, tc.expected, string(result))
		})
	}
}
