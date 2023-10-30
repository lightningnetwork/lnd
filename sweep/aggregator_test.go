package sweep

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

//nolint:lll
var (
	testInputsA = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}: &pendingInput{},
	}

	testInputsB = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &pendingInput{},
	}

	testInputsC = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &pendingInput{},
	}
)

// TestMergeClusters check that we properly can merge clusters together,
// according to their required locktime.
func TestMergeClusters(t *testing.T) {
	t.Parallel()

	lockTime1 := uint32(100)
	lockTime2 := uint32(200)

	testCases := []struct {
		name string
		a    inputCluster
		b    inputCluster
		res  []inputCluster
	}{
		{
			name: "max fee rate",
			a: inputCluster{
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					sweepFeeRate: 7000,
					inputs:       testInputsC,
				},
			},
		},
		{
			name: "same locktime",
			a: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: 7000,
					inputs:       testInputsC,
				},
			},
		},
		{
			name: "diff locktime",
			a: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				lockTime:     &lockTime2,
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: 5000,
					inputs:       testInputsA,
				},
				{
					lockTime:     &lockTime2,
					sweepFeeRate: 7000,
					inputs:       testInputsB,
				},
			},
		},
	}

	for _, test := range testCases {
		merged := mergeClusters(test.a, test.b)
		if !reflect.DeepEqual(merged, test.res) {
			t.Fatalf("[%s] unexpected result: %v",
				test.name, spew.Sdump(merged))
		}
	}
}

// TestZipClusters tests that we can merge lists of inputs clusters correctly.
func TestZipClusters(t *testing.T) {
	t.Parallel()

	createCluster := func(inp pendingInputs,
		f chainfee.SatPerKWeight) inputCluster {

		return inputCluster{
			sweepFeeRate: f,
			inputs:       inp,
		}
	}

	testCases := []struct {
		name string
		as   []inputCluster
		bs   []inputCluster
		res  []inputCluster
	}{
		{
			name: "merge A into B",
			as: []inputCluster{
				createCluster(testInputsA, 5000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 7000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 7000),
			},
		},
		{
			name: "A can't merge with B",
			as: []inputCluster{
				createCluster(testInputsA, 7000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 5000),
			},
			res: []inputCluster{
				createCluster(testInputsA, 7000),
				createCluster(testInputsB, 5000),
			},
		},
		{
			name: "empty bs",
			as: []inputCluster{
				createCluster(testInputsA, 7000),
			},
			bs: []inputCluster{},
			res: []inputCluster{
				createCluster(testInputsA, 7000),
			},
		},
		{
			name: "empty as",
			as:   []inputCluster{},
			bs: []inputCluster{
				createCluster(testInputsB, 5000),
			},
			res: []inputCluster{
				createCluster(testInputsB, 5000),
			},
		},

		{
			name: "zip 3xA into 3xB",
			as: []inputCluster{
				createCluster(testInputsA, 5000),
				createCluster(testInputsA, 5000),
				createCluster(testInputsA, 5000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 7000),
				createCluster(testInputsB, 7000),
				createCluster(testInputsB, 7000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 7000),
				createCluster(testInputsC, 7000),
				createCluster(testInputsC, 7000),
			},
		},
		{
			name: "zip A into 3xB",
			as: []inputCluster{
				createCluster(testInputsA, 2500),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 3000),
				createCluster(testInputsB, 2000),
				createCluster(testInputsB, 1000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 3000),
				createCluster(testInputsB, 2000),
				createCluster(testInputsB, 1000),
			},
		},
	}

	for _, test := range testCases {
		zipped := zipClusters(test.as, test.bs)
		if !reflect.DeepEqual(zipped, test.res) {
			t.Fatalf("[%s] unexpected result: %v",
				test.name, spew.Sdump(zipped))
		}
	}
}

// TestClusterByLockTime tests the method clusterByLockTime works as expected.
func TestClusterByLockTime(t *testing.T) {
	t.Parallel()

	// Create a mock FeePreference.
	mockFeePref := &MockFeePreference{}

	// Create a test param with a dummy fee preference. This is needed so
	// `feeRateForPreference` won't throw an error.
	param := Params{Fee: mockFeePref}

	// We begin the test by creating three clusters of inputs, the first
	// cluster has a locktime of 1, the second has a locktime of 2, and the
	// final has no locktime.
	lockTime1 := uint32(1)
	lockTime2 := uint32(2)

	// Create cluster one, which has a locktime of 1.
	input1LockTime1 := &input.MockInput{}
	input2LockTime1 := &input.MockInput{}
	input1LockTime1.On("RequiredLockTime").Return(lockTime1, true)
	input2LockTime1.On("RequiredLockTime").Return(lockTime1, true)

	// Create cluster two, which has a locktime of 2.
	input3LockTime2 := &input.MockInput{}
	input4LockTime2 := &input.MockInput{}
	input3LockTime2.On("RequiredLockTime").Return(lockTime2, true)
	input4LockTime2.On("RequiredLockTime").Return(lockTime2, true)

	// Create cluster three, which has no locktime.
	input5NoLockTime := &input.MockInput{}
	input6NoLockTime := &input.MockInput{}
	input5NoLockTime.On("RequiredLockTime").Return(uint32(0), false)
	input6NoLockTime.On("RequiredLockTime").Return(uint32(0), false)

	// With the inner Input being mocked, we can now create the pending
	// inputs.
	input1 := &pendingInput{Input: input1LockTime1, params: param}
	input2 := &pendingInput{Input: input2LockTime1, params: param}
	input3 := &pendingInput{Input: input3LockTime2, params: param}
	input4 := &pendingInput{Input: input4LockTime2, params: param}
	input5 := &pendingInput{Input: input5NoLockTime, params: param}
	input6 := &pendingInput{Input: input6NoLockTime, params: param}

	// Create the pending inputs map, which will be passed to the method
	// under test.
	//
	// NOTE: we don't care the actual outpoint values as long as they are
	// unique.
	inputs := pendingInputs{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
		wire.OutPoint{Index: 5}: input5,
		wire.OutPoint{Index: 6}: input6,
	}

	// Create expected clusters so we can shorten the line length in the
	// test cases below.
	cluster1 := pendingInputs{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
	}
	cluster2 := pendingInputs{
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
	}

	// cluster3 should be the remaining inputs since they don't have
	// locktime.
	cluster3 := pendingInputs{
		wire.OutPoint{Index: 5}: input5,
		wire.OutPoint{Index: 6}: input6,
	}

	const (
		// Set the min fee rate to be 1000 sat/kw.
		minFeeRate = chainfee.SatPerKWeight(1000)

		// Set the max fee rate to be 10,000 sat/kw.
		maxFeeRate = chainfee.SatPerKWeight(10_000)
	)

	// Create a test aggregator.
	s := NewSimpleUtxoAggregator(nil, maxFeeRate, 100)

	testCases := []struct {
		name string
		// setupMocker takes a testing fee rate and makes a mocker over
		// `Estimate` that always return the testing fee rate.
		setupMocker             func()
		testFeeRate             chainfee.SatPerKWeight
		expectedClusters        []inputCluster
		expectedRemainingInputs pendingInputs
	}{
		{
			// Test a successful case where the locktime clusters
			// are created and the no-locktime cluster is returned
			// as the remaining inputs.
			name: "successfully create clusters",
			setupMocker: func() {
				// Expect the four inputs with locktime to call
				// this method.
				mockFeePref.On("Estimate", nil, maxFeeRate).
					Return(minFeeRate+1, nil).Times(4)
			},
			// Use a fee rate above the min value so we don't hit
			// an error when performing fee estimation.
			//
			// TODO(yy): we should customize the returned fee rate
			// for each input to further test the averaging logic.
			// Or we can split the method into two, one for
			// grouping the clusters and the other for averaging
			// the fee rates so it's easier to be tested.
			testFeeRate: minFeeRate + 1,
			expectedClusters: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: minFeeRate + 1,
					inputs:       cluster1,
				},
				{
					lockTime:     &lockTime2,
					sweepFeeRate: minFeeRate + 1,
					inputs:       cluster2,
				},
			},
			expectedRemainingInputs: cluster3,
		},
		{
			// Test that when the input is skipped when the fee
			// estimation returns an error.
			name: "error from fee estimation",
			setupMocker: func() {
				mockFeePref.On("Estimate", nil, maxFeeRate).
					Return(chainfee.SatPerKWeight(0),
						errors.New("dummy")).Times(4)
			},

			// Use a fee rate below the min value so we hit an
			// error when performing fee estimation.
			testFeeRate:      minFeeRate - 1,
			expectedClusters: []inputCluster{},
			// Remaining inputs should stay untouched.
			expectedRemainingInputs: cluster3,
		},
	}

	//nolint:paralleltest
	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Apply the test fee rate so `feeRateForPreference` is
			// mocked to return the specified value.
			tc.setupMocker()

			// Assert the mocked methods are called as expeceted.
			defer mockFeePref.AssertExpectations(t)

			// Call the method under test.
			clusters, remainingInputs := s.clusterByLockTime(inputs)

			// Sort by locktime as the order is not guaranteed.
			sort.Slice(clusters, func(i, j int) bool {
				return *clusters[i].lockTime <
					*clusters[j].lockTime
			})

			// Validate the values are returned as expected.
			require.Equal(t, tc.expectedClusters, clusters)
			require.Equal(t, tc.expectedRemainingInputs,
				remainingInputs,
			)

			// Assert the mocked methods are called as expeceted.
			input1LockTime1.AssertExpectations(t)
			input2LockTime1.AssertExpectations(t)
			input3LockTime2.AssertExpectations(t)
			input4LockTime2.AssertExpectations(t)
			input5NoLockTime.AssertExpectations(t)
			input6NoLockTime.AssertExpectations(t)
		})
	}
}
