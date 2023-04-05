package main

import (
	"errors"
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
			errors.New("unable to decode output index: strconv." +
				"ParseInt: parsing \"string\": invalid syntax"),
		}, {
			"not_hex:0",
			true,
			0,
			errors.New("unable to parse hex string: encoding/hex:" +
				" invalid byte: U+006E 'n'"),
		},
	}
	for _, tc := range testCases {
		cp, err := parseChanPoint(tc.channelPoinStr)
		require.Equal(t, tc.err, err)
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
