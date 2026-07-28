package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestSimLatencyValidate pins what a latency section may say. The refusals
// matter as much as the acceptances: a section that charges nothing is not the
// flat tick, and hold_carry false would be a latency knob editing the traffic
// engine.
func TestSimLatencyValidate(t *testing.T) {
	t.Parallel()

	yes, no := true, false

	tests := []struct {
		name   string
		params *SimLatencyParams
		err    string
	}{
		{
			name:   "absent",
			params: nil,
		},
		{
			name: "both set",
			params: &SimLatencyParams{
				PerHopMs:          300,
				AttemptOverheadMs: 250,
			},
		},
		{
			name:   "per hop alone",
			params: &SimLatencyParams{PerHopMs: 300},
		},
		{
			name:   "overhead alone",
			params: &SimLatencyParams{AttemptOverheadMs: 250},
		},
		{
			name: "hold carry true",
			params: &SimLatencyParams{
				PerHopMs:  300,
				HoldCarry: &yes,
			},
		},
		{
			name:   "negative per hop",
			params: &SimLatencyParams{PerHopMs: -1},
			err:    "per_hop_ms must not be negative",
		},
		{
			name:   "negative overhead",
			params: &SimLatencyParams{AttemptOverheadMs: -1},
			err:    "attempt_overhead_ms must not be negative",
		},
		{
			name:   "free attempts",
			params: &SimLatencyParams{},
			err:    "omit the section",
		},
		{
			name: "hold carry false",
			params: &SimLatencyParams{
				PerHopMs:  300,
				HoldCarry: &no,
			},
			err: "hold_carry false is REFUSED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.params.validate()
			if test.err == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.err)
		})
	}
}

// TestSimLatencyReturnTrip pins the arithmetic: a hop is charged twice, out and
// back, and a route nobody crossed is charged nothing.
func TestSimLatencyReturnTrip(t *testing.T) {
	t.Parallel()

	latency, err := newSimLatency(&SimLatencyParams{
		PerHopMs:          300,
		AttemptOverheadMs: 250,
	})
	require.NoError(t, err)

	require.Equal(t, 250*time.Millisecond, latency.overhead)
	require.Equal(t, time.Duration(0), latency.returnTrip(0))
	require.Equal(t, 600*time.Millisecond, latency.returnTrip(1))
	require.Equal(t, 4800*time.Millisecond, latency.returnTrip(8))
}

// TestSimLatencyHops pins the differential structure, which is the whole reason
// this stage is not obviously exp-019's null: a failure is charged for the hops
// the htlc actually crossed, and a settle for the whole route.
func TestSimLatencyHops(t *testing.T) {
	t.Parallel()

	rt := attributionTestRoute(t, 8)

	// A settle traversed the whole route.
	require.Equal(t, 8, simLatencyHops(rt, SimHtlcResult{}))

	// The sender's own first hop costs one round trip, not zero: the spec's
	// own reading, and the one that keeps a first-hop probe from being free
	// in time.
	require.Equal(t, 1, simLatencyHops(rt, attributionTestFailure(rt, 0)))

	// A failure reported by the node at index i is a failure of hop i+1.
	require.Equal(t, 2, simLatencyHops(rt, attributionTestFailure(rt, 1)))
	require.Equal(t, 8, simLatencyHops(rt, attributionTestFailure(rt, 7)))

	// The final node has no further hop to charge for, so the route length
	// is the cap.
	require.Equal(t, 8, simLatencyHops(rt, attributionTestFailure(rt, 8)))

	// A failure from a node that is not on the route at all is charged the
	// whole route rather than credited with stopping early.
	require.Equal(t, 8, simLatencyHops(rt, SimHtlcResult{
		FailureSource: SimNodePubKey(999),
		Failure:       &lnwire.FailTemporaryChannelFailure{},
	}))
}
