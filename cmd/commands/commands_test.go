package commands

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TestParseBlockHeightInputs tests the input block heights and ensure that the
// proper errors are returned when they could lead in invalid results.
func TestParseBlockHeightInputs(t *testing.T) {
	t.Parallel()

	app := cli.NewApp()

	startDefault, endDefault := int64(0), int64(-1)

	testCases := []struct {
		name          string
		expectedStart int32
		expectedEnd   int32
		expectedErr   string
	}{
		{
			name:          "start less than end",
			expectedStart: 100,
			expectedEnd:   200,
			expectedErr:   "",
		},
		{
			name:          "start greater than end",
			expectedStart: 200,
			expectedEnd:   100,
			expectedErr: "start_height should be " +
				"less than end_height if end_height is " +
				"not equal to -1",
		},
		{
			name:          "only start height set",
			expectedStart: 100,
			expectedEnd:   -1,
			expectedErr:   "",
		},
		{
			name:          "start set and end height set as -1",
			expectedStart: 100,
			expectedEnd:   -1,
			expectedErr:   "",
		},
		{
			name:          "neither start nor end heights defined",
			expectedStart: 0,
			expectedEnd:   -1,
			expectedErr:   "",
		},
		{
			name:          "only end height defined",
			expectedStart: 0,
			expectedEnd:   100,
			expectedErr:   "",
		},
		{
			name:          "start height is a negative",
			expectedStart: -1,
			expectedEnd:   100,
			expectedErr: "start_height should be greater " +
				"than or equal to 0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			flagSet := flag.NewFlagSet(
				"listchaintxns", flag.ContinueOnError,
			)

			var startHeight, endHeight int64
			flagSet.Int64Var(
				&startHeight, "start_height", startDefault, "",
			)
			flagSet.Int64Var(
				&endHeight, "end_height", endDefault, "",
			)

			err := flagSet.Set(
				"start_height",
				strconv.Itoa(int(tc.expectedStart)),
			)
			require.NoError(
				t, err, "failed to set start_height flag",
			)

			err = flagSet.Set(
				"end_height", strconv.Itoa(int(tc.expectedEnd)),
			)
			require.NoError(
				t, err, "failed to set end_height flag",
			)

			ctx := cli.NewContext(app, flagSet, nil)
			start, end, err := parseBlockHeightInputs(ctx)
			if tc.expectedErr != "" {
				require.EqualError(t, err, tc.expectedErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.expectedStart, start)
			require.Equal(t, tc.expectedEnd, end)
		})
	}
}

// TestParseChanIDs tests the parseChanIDs function with various
// valid and invalid input values and verifies the output.
func TestParseChanIDs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		chanIDs     []string
		expected    []uint64
		expectedErr bool
	}{
		{
			name: "valid chan ids",
			chanIDs: []string{
				"1499733860352000", "17592186044552773633",
			},
			expected: []uint64{
				1499733860352000, 17592186044552773633,
			},
			expectedErr: false,
		},
		{
			name: "invalid chan id",
			chanIDs: []string{
				"channel id",
			},
			expected:    []uint64{},
			expectedErr: true,
		},
		{
			name: "negative chan id",
			chanIDs: []string{
				"-10000",
			},
			expected:    []uint64{},
			expectedErr: true,
		},
		{
			name:        "empty chan ids",
			chanIDs:     []string{},
			expected:    nil,
			expectedErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chanIDs, err := parseChanIDs(tc.chanIDs)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, chanIDs)
		})
	}
}

// TestParsePolicyUpdateRequest checks that updatechanpolicy only marks the
// fields the user passed as specified.
func TestParsePolicyUpdateRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		want   *lnrpc.PolicyUpdateRequest
		errStr string
	}{
		{
			name: "fee rate ppm only",
			args: []string{"--fee_rate_ppm=0"},
			want: &lnrpc.PolicyUpdateRequest{
				PartialUpdate:    true,
				FeeRateSpecified: true,
			},
		},
		{
			name: "base fee zero only",
			args: []string{"--base_fee_msat=0"},
			want: &lnrpc.PolicyUpdateRequest{
				PartialUpdate:        true,
				BaseFeeMsatSpecified: true,
			},
		},
		{
			name: "time lock delta only",
			args: []string{"--time_lock_delta=80"},
			want: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: true,
				TimeLockDelta: 80,
			},
		},
		{
			name: "all positional",
			args: []string{"1000", "0.001", "40"},
			want: &lnrpc.PolicyUpdateRequest{
				PartialUpdate:        true,
				BaseFeeMsat:          1000,
				BaseFeeMsatSpecified: true,
				FeeRate:              0.001,
				FeeRateSpecified:     true,
				TimeLockDelta:        40,
			},
		},
		{
			name:   "nothing to update",
			args:   []string{},
			errStr: "no policy fields to update",
		},
		{
			name:   "explicit zero time lock delta",
			args:   []string{"--time_lock_delta=0"},
			errStr: "time_lock_delta must be greater than 0",
		},
		{
			name:   "invalid base fee",
			args:   []string{"--base_fee_msat=abc"},
			errStr: "unable to decode base_fee_msat",
		},
		{
			name:   "invalid fee rate ppm",
			args:   []string{"--fee_rate_ppm=-1"},
			errStr: "unable to decode fee_rate_ppm",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flagSet := flag.NewFlagSet(
				"updatechanpolicy", flag.ContinueOnError,
			)
			for _, f := range updateChannelPolicyCommand.Flags {
				f.Apply(flagSet)
			}
			require.NoError(t, flagSet.Parse(tc.args))

			ctx := cli.NewContext(nil, flagSet, nil)
			got, err := parsePolicyUpdateRequest(ctx)
			if tc.errStr != "" {
				require.ErrorContains(t, err, tc.errStr)
				return
			}
			require.NoError(t, err)

			tc.want.Scope = &lnrpc.PolicyUpdateRequest_Global{
				Global: true,
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// TestSupportsPartialPolicyUpdate checks the lnd version gate for partial
// channel policy updates.
func TestSupportsPartialPolicyUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		want    bool
	}{
		{"0.21.6-beta commit=v0.21.6-beta", false},
		{"0.20.1-beta", false},
		{"0.21.99-beta commit=v0.21.0-beta-500-gabc", true},
		{"0.22.0-beta commit=v0.22.0-beta", true},
		{"0.23.1-beta", true},
		{"1.0.0", true},
		{"", false},
		{"garbage", false},
	}

	for _, tc := range tests {
		require.Equal(
			t, tc.want, supportsPartialPolicyUpdate(tc.version),
			tc.version,
		)
	}
}

// getInfoClient is a LightningClient whose GetInfo returns a fixed result.
type getInfoClient struct {
	lnrpc.LightningClient

	info  *lnrpc.GetInfoResponse
	err   error
	calls int
}

func (c *getInfoClient) GetInfo(context.Context, *lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	c.calls++
	return c.info, c.err
}

// TestCheckPartialPolicyUpdate checks when lncli refuses a partial policy
// update because lnd may not support it.
func TestCheckPartialPolicyUpdate(t *testing.T) {
	t.Parallel()

	partial := &lnrpc.PolicyUpdateRequest{
		PartialUpdate: true,
		TimeLockDelta: 80,
	}
	oldLnd := &lnrpc.GetInfoResponse{Version: "0.21.6-beta"}
	newLnd := &lnrpc.GetInfoResponse{Version: "0.22.0-beta"}
	denied := status.Error(codes.PermissionDenied, "permission denied")

	tests := []struct {
		name      string
		req       *lnrpc.PolicyUpdateRequest
		client    *getInfoClient
		wantCalls int
		errStr    string
	}{
		{
			name: "no time lock delta skips check",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate:    true,
				FeeRateSpecified: true,
			},
			client: &getInfoClient{err: denied},
		},
		{
			name: "all fee fields skips check",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate:        true,
				TimeLockDelta:        80,
				BaseFeeMsatSpecified: true,
				FeeRateSpecified:     true,
			},
			client: &getInfoClient{err: denied},
		},
		{
			name:      "new lnd",
			req:       partial,
			client:    &getInfoClient{info: newLnd},
			wantCalls: 1,
		},
		{
			name:      "old lnd",
			req:       partial,
			client:    &getInfoClient{info: oldLnd},
			wantCalls: 1,
			errStr:    "doesn't support partial policy updates",
		},
		{
			name:      "no info permission",
			req:       partial,
			client:    &getInfoClient{err: denied},
			wantCalls: 1,
			errStr:    "needs info:read permission",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPartialPolicyUpdate(
				t.Context(), tc.client, tc.req,
			)
			require.Equal(t, tc.wantCalls, tc.client.calls)
			if tc.errStr != "" {
				require.ErrorContains(t, err, tc.errStr)
				return
			}
			require.NoError(t, err)
		})
	}
}
