package chanacceptor

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestMergeResponse tests merging of channel acceptor responses.
func TestMergeResponse(t *testing.T) {
	var (
		addr1 = lnwire.DeliveryAddress{1}
		addr2 = lnwire.DeliveryAddress{2}

		populatedResp = ChannelAcceptResponse{
			UpfrontShutdown: addr1,
			CSVDelay:        2,
			Reserve:         3,
			InFlightTotal:   4,
			HtlcLimit:       5,
			MinHtlcIn:       6,
			MinAcceptDepth:  7,
		}
	)

	tests := []struct {
		name    string
		current ChannelAcceptResponse
		new     ChannelAcceptResponse
		merged  ChannelAcceptResponse
		err     error
	}{
		{
			name:    "same response",
			current: populatedResp,
			new:     populatedResp,
			merged:  populatedResp,
			err:     nil,
		},
		{
			name: "different upfront",
			current: ChannelAcceptResponse{
				UpfrontShutdown: addr1,
			},
			new: ChannelAcceptResponse{
				UpfrontShutdown: addr2,
			},
			err: fieldMismatchError(fieldUpfrontShutdown, addr1, addr2),
		},
		{
			name: "different csv",
			current: ChannelAcceptResponse{
				CSVDelay: 1,
			},
			new: ChannelAcceptResponse{
				CSVDelay: 2,
			},
			err: fieldMismatchError(fieldCSV, 1, 2),
		},
		{
			name: "different reserve",
			current: ChannelAcceptResponse{
				Reserve: 1,
			},
			new: ChannelAcceptResponse{
				Reserve: 2,
			},
			err: fieldMismatchError(fieldReserve, 1, 2),
		},
		{
			name: "different in flight",
			current: ChannelAcceptResponse{
				InFlightTotal: 1,
			},
			new: ChannelAcceptResponse{
				InFlightTotal: 2,
			},
			err: fieldMismatchError(
				fieldInFlightTotal, lnwire.MilliSatoshi(1),
				lnwire.MilliSatoshi(2),
			),
		},
		{
			name: "different htlc limit",
			current: ChannelAcceptResponse{
				HtlcLimit: 1,
			},
			new: ChannelAcceptResponse{
				HtlcLimit: 2,
			},
			err: fieldMismatchError(fieldHtlcLimit, 1, 2),
		},
		{
			name: "different min in",
			current: ChannelAcceptResponse{
				MinHtlcIn: 1,
			},
			new: ChannelAcceptResponse{
				MinHtlcIn: 2,
			},
			err: fieldMismatchError(
				fieldMinIn, lnwire.MilliSatoshi(1),
				lnwire.MilliSatoshi(2),
			),
		},
		{
			name: "different depth",
			current: ChannelAcceptResponse{
				MinAcceptDepth: 1,
			},
			new: ChannelAcceptResponse{
				MinAcceptDepth: 2,
			},
			err: fieldMismatchError(fieldMinDep, 1, 2),
		},
		{
			name: "merge all values",
			current: ChannelAcceptResponse{
				UpfrontShutdown: lnwire.DeliveryAddress{1},
				CSVDelay:        1,
				Reserve:         0,
				InFlightTotal:   3,
				HtlcLimit:       0,
				MinHtlcIn:       5,
				MinAcceptDepth:  0,
			},
			new: ChannelAcceptResponse{
				UpfrontShutdown: nil,
				CSVDelay:        0,
				Reserve:         2,
				InFlightTotal:   0,
				HtlcLimit:       4,
				MinHtlcIn:       0,
				MinAcceptDepth:  6,
			},
			merged: ChannelAcceptResponse{
				UpfrontShutdown: lnwire.DeliveryAddress{1},
				CSVDelay:        1,
				Reserve:         2,
				InFlightTotal:   3,
				HtlcLimit:       4,
				MinHtlcIn:       5,
				MinAcceptDepth:  6,
			},
			err: nil,
		},
		{
			// Test the case where fields have the same non-zero
			// value, and the case where only response value is
			// non-zero.
			name: "empty and identical",
			current: ChannelAcceptResponse{
				CSVDelay:      1,
				Reserve:       2,
				InFlightTotal: 0,
			},
			new: ChannelAcceptResponse{
				CSVDelay:      0,
				Reserve:       2,
				InFlightTotal: 3,
			},
			merged: ChannelAcceptResponse{
				CSVDelay:      1,
				Reserve:       2,
				InFlightTotal: 3,
			},
			err: nil,
		},
		{
			// Test the case where one response has ZeroConf set
			// and another has a non-zero min depth set.
			name: "zero conf conflict",
			current: ChannelAcceptResponse{
				ZeroConf: true,
			},
			new: ChannelAcceptResponse{
				MinAcceptDepth: 5,
			},
			err: errZeroConf,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			resp, err := mergeResponse(test.current, test.new)
			require.Equal(t, test.err, err)

			// If we expect an error, exit early rather than compare
			// our result.
			if test.err != nil {
				return
			}

			require.Equal(t, test.merged, resp)
		})
	}
}
