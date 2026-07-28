package routing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSimConcurrencyParamsValidate asserts that the section rejects what the
// scheduler cannot honor, and that the deferred arrival process says so by
// name rather than quietly running the one that is implemented.
func TestSimConcurrencyParamsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params *SimConcurrencyParams
		errStr string
	}{
		{
			name:   "absent section",
			params: nil,
		},
		{
			name:   "sequential",
			params: &SimConcurrencyParams{MaxInFlight: 1},
		},
		{
			name: "window named",
			params: &SimConcurrencyParams{
				MaxInFlight:     4,
				Arrival:         "window",
				InterArrivalSec: 30,
			},
		},
		{
			name:   "zero window",
			params: &SimConcurrencyParams{MaxInFlight: 0},
			errStr: "max_in_flight must be positive",
		},
		{
			name:   "negative window",
			params: &SimConcurrencyParams{MaxInFlight: -1},
			errStr: "max_in_flight must be positive",
		},
		{
			name: "poisson is deferred by name",
			params: &SimConcurrencyParams{
				MaxInFlight: 4,
				Arrival:     "poisson",
			},
			errStr: "DEFERRED",
		},
		{
			name: "unknown arrival",
			params: &SimConcurrencyParams{
				MaxInFlight: 4,
				Arrival:     "batch",
			},
			errStr: "unknown arrival",
		},
		{
			name: "negative inter arrival",
			params: &SimConcurrencyParams{
				MaxInFlight:     2,
				InterArrivalSec: -1,
			},
			errStr: "must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.params.validate()
			if test.errStr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.errStr)
		})
	}
}

// TestSimConcurrencyMaxInFlightDefault asserts that an absent section is the
// sequential batch, which is what every scenario file written before stage D
// says by omission.
func TestSimConcurrencyMaxInFlightDefault(t *testing.T) {
	t.Parallel()

	var absent *SimConcurrencyParams
	require.Equal(t, 1, absent.maxInFlight())

	present := &SimConcurrencyParams{MaxInFlight: 4}
	require.Equal(t, 4, present.maxInFlight())
}
