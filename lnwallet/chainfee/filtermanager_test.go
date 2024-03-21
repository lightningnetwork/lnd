package chainfee

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFeeFilterMedian(t *testing.T) {
	tests := []struct {
		name           string
		fetchedFilters []SatPerKWeight
		expectedMedian SatPerKWeight
		expectedErr    error
	}{
		{
			name:           "no filter data",
			fetchedFilters: nil,
			expectedErr:    errNoData,
		},
		{
			name: "even number of filters",
			fetchedFilters: []SatPerKWeight{
				15000, 18000, 19000, 25000,
			},
			expectedMedian: SatPerKWeight(18500),
		},
		{
			name: "odd number of filters",
			fetchedFilters: []SatPerKWeight{
				15000, 18000, 25000,
			},
			expectedMedian: SatPerKWeight(18000),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			cb := func() ([]SatPerKWeight, error) {
				return nil, nil
			}
			fm := newFilterManager(cb)

			fm.updateMedian(test.fetchedFilters)

			median, err := fm.FetchMedianFilter()
			require.Equal(t, test.expectedErr, err)
			require.Equal(t, test.expectedMedian, median)
		})
	}
}
