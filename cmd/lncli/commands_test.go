package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
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

// TestParseRouteHints tests parseRouteHints with various
// valid and invalid input values and verifies the output.
func TestParseRouteHints(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		routeHints  string
		expected    []*lnrpc.HopHint
		expectError bool
		errorMsg    string
	}{
		{
			name: "InvalidChanID",
			routeHints: `[{"node_id":"0323...", ` +
				`"chan_id":"invalid"}]`,
			expectError: true,
			errorMsg: "error parsing route hints JSON: " +
				"json: cannot unmarshal string into " +
				"Go struct field HopHint.chan_id of " +
				"type uint64",
		},
		{
			name: "ValidSingleHint",
			routeHints: `[{"node_id":"0323...", ` +
				`"chan_id":1234567890, ` +
				`"fee_base_msat":1000}]`,
			expected: []*lnrpc.HopHint{
				{
					NodeId:      "0323...",
					ChanId:      1234567890,
					FeeBaseMsat: 1000,
				},
			},
			expectError: false,
		},
		{
			name: "ValidMultipleHints",
			routeHints: `[{"node_id":"0323...", ` +
				`"chan_id":1234567890, ` +
				`"fee_base_msat":1000}, ` +
				`{"node_id":"0245...", ` +
				`"chan_id":987654321, ` +
				`"fee_base_msat":2000}]`,
			expected: []*lnrpc.HopHint{
				{
					NodeId:      "0323...",
					ChanId:      1234567890,
					FeeBaseMsat: 1000,
				},
				{
					NodeId:      "0245...",
					ChanId:      987654321,
					FeeBaseMsat: 2000,
				},
			},
			expectError: false,
		},
		{
			name:        "MissingChanID",
			routeHints:  `[{"node_id":"0323..."}]`,
			expectError: true,
			errorMsg: "chan_id is missing or invalid in route " +
				"hint",
		},
		{
			name:        "EmptyJSON",
			routeHints:  `[]`,
			expectError: true,
			errorMsg:    "no valid route hints found in JSON",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			invoiceHints, err := parseRouteHints(tc.routeHints)
			if tc.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expected, invoiceHints)
			}
		})
	}
}
