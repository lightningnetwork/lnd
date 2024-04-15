package sweep

import (
	"bytes"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

//nolint:lll
var (
	testInputsA = InputsMap{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}: &SweeperInput{},
	}

	testInputsB = InputsMap{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &SweeperInput{},
	}

	testInputsC = InputsMap{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}:  &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}:  &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}:  &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &SweeperInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &SweeperInput{},
	}

	testHeight = int32(800000)
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

	createCluster := func(inp InputsMap,
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
	input1 := &SweeperInput{Input: input1LockTime1, params: param}
	input2 := &SweeperInput{Input: input2LockTime1, params: param}
	input3 := &SweeperInput{Input: input3LockTime2, params: param}
	input4 := &SweeperInput{Input: input4LockTime2, params: param}
	input5 := &SweeperInput{Input: input5NoLockTime, params: param}
	input6 := &SweeperInput{Input: input6NoLockTime, params: param}

	// Create the pending inputs map, which will be passed to the method
	// under test.
	//
	// NOTE: we don't care the actual outpoint values as long as they are
	// unique.
	inputs := InputsMap{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
		wire.OutPoint{Index: 5}: input5,
		wire.OutPoint{Index: 6}: input6,
	}

	// Create expected clusters so we can shorten the line length in the
	// test cases below.
	cluster1 := InputsMap{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
	}
	cluster2 := InputsMap{
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
	}

	// cluster3 should be the remaining inputs since they don't have
	// locktime.
	cluster3 := InputsMap{
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
		expectedRemainingInputs InputsMap
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

// TestBudgetAggregatorFilterInputs checks that inputs with low budget are
// filtered out.
func TestBudgetAggregatorFilterInputs(t *testing.T) {
	t.Parallel()

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create a mock WitnessType that always return an error when trying to
	// get its size upper bound.
	wtErr := &input.MockWitnessType{}
	defer wtErr.AssertExpectations(t)

	// Mock the `SizeUpperBound` method to return an error exactly once.
	dummyErr := errors.New("dummy error")
	wtErr.On("SizeUpperBound").Return(0, false, dummyErr).Once()

	// Create a mock WitnessType that gives the size.
	wt := &input.MockWitnessType{}
	defer wt.AssertExpectations(t)

	// Mock the `SizeUpperBound` method to return the size four times.
	const wtSize = 100
	wt.On("SizeUpperBound").Return(wtSize, true, nil).Times(4)

	// Create a mock input that will be filtered out due to error.
	inpErr := &input.MockInput{}
	defer inpErr.AssertExpectations(t)

	// Mock the `WitnessType` method to return the erroring witness type.
	inpErr.On("WitnessType").Return(wtErr).Once()

	// Mock the `OutPoint` method to return a unique outpoint.
	opErr := wire.OutPoint{Hash: chainhash.Hash{1}}
	inpErr.On("OutPoint").Return(opErr).Once()

	// Mock the estimator to return a constant fee rate.
	const minFeeRate = chainfee.SatPerKWeight(1000)
	estimator.On("RelayFeePerKW").Return(minFeeRate).Once()

	var (
		// Define three budget values, one below the min fee rate, one
		// above and one equal to it.
		budgetLow   = minFeeRate.FeeForWeight(wtSize) - 1
		budgetEqual = minFeeRate.FeeForWeight(wtSize)
		budgetHigh  = minFeeRate.FeeForWeight(wtSize) + 1

		// Define three outpoints with different budget values.
		opLow   = wire.OutPoint{Hash: chainhash.Hash{2}}
		opEqual = wire.OutPoint{Hash: chainhash.Hash{3}}
		opHigh  = wire.OutPoint{Hash: chainhash.Hash{4}}

		// Define an outpoint that has a dust required output.
		opDust = wire.OutPoint{Hash: chainhash.Hash{5}}
	)

	// Create three mock inputs.
	inpLow := &input.MockInput{}
	defer inpLow.AssertExpectations(t)

	inpEqual := &input.MockInput{}
	defer inpEqual.AssertExpectations(t)

	inpHigh := &input.MockInput{}
	defer inpHigh.AssertExpectations(t)

	inpDust := &input.MockInput{}
	defer inpDust.AssertExpectations(t)

	// Mock the `WitnessType` method to return the witness type.
	inpLow.On("WitnessType").Return(wt)
	inpEqual.On("WitnessType").Return(wt)
	inpHigh.On("WitnessType").Return(wt)
	inpDust.On("WitnessType").Return(wt)

	// Mock the `OutPoint` method to return the unique outpoint.
	inpLow.On("OutPoint").Return(opLow)
	inpEqual.On("OutPoint").Return(opEqual)
	inpHigh.On("OutPoint").Return(opHigh)
	inpDust.On("OutPoint").Return(opDust)

	// Mock the `RequiredTxOut` to return nil.
	inpEqual.On("RequiredTxOut").Return(nil)
	inpHigh.On("RequiredTxOut").Return(nil)

	// Mock the dust required output.
	inpDust.On("RequiredTxOut").Return(&wire.TxOut{
		Value:    0,
		PkScript: bytes.Repeat([]byte{0}, input.P2WSHSize),
	})

	// Create testing pending inputs.
	inputs := InputsMap{
		// The first input will be filtered out due to the error.
		opErr: &SweeperInput{
			Input: inpErr,
		},

		// The second input will be filtered out due to the budget.
		opLow: &SweeperInput{
			Input:  inpLow,
			params: Params{Budget: budgetLow},
		},

		// The third input will be included.
		opEqual: &SweeperInput{
			Input:  inpEqual,
			params: Params{Budget: budgetEqual},
		},

		// The fourth input will be included.
		opHigh: &SweeperInput{
			Input:  inpHigh,
			params: Params{Budget: budgetHigh},
		},

		// The fifth input will be filtered out due to the dust
		// required.
		opDust: &SweeperInput{
			Input:  inpDust,
			params: Params{Budget: budgetHigh},
		},
	}

	// Init the budget aggregator with the mocked estimator and zero max
	// num of inputs.
	b := NewBudgetAggregator(estimator, 0)

	// Call the method under test.
	result := b.filterInputs(inputs)

	// Validate the expected inputs are returned.
	require.Len(t, result, 2)

	// We expect only the inputs with budget equal or above the min fee to
	// be included.
	require.Contains(t, result, opEqual)
	require.Contains(t, result, opHigh)
}

// TestBudgetAggregatorSortInputs checks that inputs are sorted by based on
// their budgets and force flag.
func TestBudgetAggregatorSortInputs(t *testing.T) {
	t.Parallel()

	var (
		// Create two budgets.
		budgetLow   = btcutil.Amount(1000)
		budgetHight = budgetLow + btcutil.Amount(1000)
	)

	// Create an input with the low budget but forced.
	inputLowForce := SweeperInput{
		params: Params{
			Budget:    budgetLow,
			Immediate: true,
		},
	}

	// Create an input with the low budget.
	inputLow := SweeperInput{
		params: Params{
			Budget: budgetLow,
		},
	}

	// Create an input with the high budget and forced.
	inputHighForce := SweeperInput{
		params: Params{
			Budget:    budgetHight,
			Immediate: true,
		},
	}

	// Create an input with the high budget.
	inputHigh := SweeperInput{
		params: Params{
			Budget: budgetHight,
		},
	}

	// Create a testing pending inputs.
	inputs := []SweeperInput{
		inputLowForce,
		inputLow,
		inputHighForce,
		inputHigh,
	}

	// Init the budget aggregator with zero max num of inputs.
	b := NewBudgetAggregator(nil, 0)

	// Call the method under test.
	result := b.sortInputs(inputs)
	require.Len(t, result, 4)

	// The first input should be the forced input with the high budget.
	require.Equal(t, inputHighForce, result[0])

	// The second input should be the forced input with the low budget.
	require.Equal(t, inputLowForce, result[1])

	// The third input should be the input with the high budget.
	require.Equal(t, inputHigh, result[2])

	// The fourth input should be the input with the low budget.
	require.Equal(t, inputLow, result[3])
}

// TestBudgetAggregatorCreateInputSets checks that the budget aggregator
// creates input sets when the number of inputs exceeds the max number
// configed.
func TestBudgetAggregatorCreateInputSets(t *testing.T) {
	t.Parallel()

	// Create mocks input that doesn't have required outputs.
	mockInput1 := &input.MockInput{}
	defer mockInput1.AssertExpectations(t)
	mockInput2 := &input.MockInput{}
	defer mockInput2.AssertExpectations(t)
	mockInput3 := &input.MockInput{}
	defer mockInput3.AssertExpectations(t)
	mockInput4 := &input.MockInput{}
	defer mockInput4.AssertExpectations(t)

	// Create testing pending inputs.
	pi1 := SweeperInput{
		Input: mockInput1,
		params: Params{
			DeadlineHeight: fn.Some(testHeight),
		},
	}
	pi2 := SweeperInput{
		Input: mockInput2,
		params: Params{
			DeadlineHeight: fn.Some(testHeight),
		},
	}
	pi3 := SweeperInput{
		Input: mockInput3,
		params: Params{
			DeadlineHeight: fn.Some(testHeight),
		},
	}
	pi4 := SweeperInput{
		Input: mockInput4,
		params: Params{
			// This input has a deadline height that is different
			// from the other inputs. When grouped with other
			// inputs, it will cause an error to be returned.
			DeadlineHeight: fn.Some(testHeight + 1),
		},
	}

	// Create a budget aggregator with max number of inputs set to 2.
	b := NewBudgetAggregator(nil, 2)

	// Create test cases.
	testCases := []struct {
		name            string
		inputs          []SweeperInput
		setupMock       func()
		expectedNumSets int
	}{
		{
			// When the number of inputs is below the max, a single
			// input set is returned.
			name:   "num inputs below max",
			inputs: []SweeperInput{pi1},
			setupMock: func() {
				// Mock methods used in loggings.
				mockInput1.On("WitnessType").Return(
					input.CommitmentAnchor)
				mockInput1.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{1}})
			},
			expectedNumSets: 1,
		},
		{
			// When the number of inputs is equal to the max, a
			// single input set is returned.
			name:   "num inputs equal to max",
			inputs: []SweeperInput{pi1, pi2},
			setupMock: func() {
				// Mock methods used in loggings.
				mockInput1.On("WitnessType").Return(
					input.CommitmentAnchor)
				mockInput2.On("WitnessType").Return(
					input.CommitmentAnchor)

				mockInput1.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{1}})
				mockInput2.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{2}})
			},
			expectedNumSets: 1,
		},
		{
			// When the number of inputs is above the max, multiple
			// input sets are returned.
			name:   "num inputs above max",
			inputs: []SweeperInput{pi1, pi2, pi3},
			setupMock: func() {
				// Mock methods used in loggings.
				mockInput1.On("WitnessType").Return(
					input.CommitmentAnchor)
				mockInput2.On("WitnessType").Return(
					input.CommitmentAnchor)
				mockInput3.On("WitnessType").Return(
					input.CommitmentAnchor)

				mockInput1.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{1}})
				mockInput2.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{2}})
				mockInput3.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{3}})
			},
			expectedNumSets: 2,
		},
		{
			// When the number of inputs is above the max, but an
			// error is returned from creating the first set, it
			// shouldn't affect the remaining inputs.
			name:   "num inputs above max with error",
			inputs: []SweeperInput{pi1, pi4, pi3},
			setupMock: func() {
				// Mock methods used in loggings.
				mockInput1.On("WitnessType").Return(
					input.CommitmentAnchor)
				mockInput3.On("WitnessType").Return(
					input.CommitmentAnchor)

				mockInput1.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{1}})
				mockInput3.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{3}})
				mockInput4.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{2}})
			},
			expectedNumSets: 1,
		},
	}

	// Iterate over the test cases.
	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Setup the mocks.
			tc.setupMock()

			// Call the method under test.
			result := b.createInputSets(tc.inputs, testHeight)

			// Validate the expected number of input sets are
			// returned.
			require.Len(t, result, tc.expectedNumSets)
		})
	}
}

// TestBudgetInputSetClusterInputs checks that the budget aggregator clusters
// inputs into input sets based on their deadline heights.
func TestBudgetInputSetClusterInputs(t *testing.T) {
	t.Parallel()

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create a mock WitnessType that gives the size.
	wt := &input.MockWitnessType{}
	defer wt.AssertExpectations(t)

	// Mock the `SizeUpperBound` method to return the size 10 times since
	// we are using ten inputs.
	const wtSize = 100
	wt.On("SizeUpperBound").Return(wtSize, true, nil).Times(10)
	wt.On("String").Return("mock witness type")

	// Mock the estimator to return a constant fee rate.
	const minFeeRate = chainfee.SatPerKWeight(1000)
	estimator.On("RelayFeePerKW").Return(minFeeRate).Once()

	var (
		// Define two budget values, one below the min fee rate and one
		// above it.
		budgetLow  = minFeeRate.FeeForWeight(wtSize) - 1
		budgetHigh = minFeeRate.FeeForWeight(wtSize) + 1

		// Create three deadline heights, which means there are three
		// groups of inputs to be expected.
		defaultDeadline = testHeight + DefaultDeadlineDelta
		deadline1       = int32(1)
		deadline2       = int32(2)
	)

	// Create testing pending inputs.
	inputs := make(InputsMap)

	// Create a mock input that is exclusive.
	inpExclusive := &input.MockInput{}
	defer inpExclusive.AssertExpectations(t)

	// We expect the high budget input to call this method three times,
	// 1. in `filterInputs`
	// 2. in `createInputSet`
	// 3. when assigning the input to the exclusiveInputs.
	// 4. when iterating the exclusiveInputs.
	opExclusive := wire.OutPoint{Hash: chainhash.Hash{1, 2, 3, 4, 5}}
	inpExclusive.On("OutPoint").Return(opExclusive).Maybe()

	// Mock the `WitnessType` method to return the witness type.
	inpExclusive.On("WitnessType").Return(wt)

	// Mock the `RequiredTxOut` to return nil.
	inpExclusive.On("RequiredTxOut").Return(nil)

	// Add the exclusive input to the inputs map. We expect this input to
	// be in its own input set although it has deadline1.
	exclusiveGroup := uint64(123)
	inputs[opExclusive] = &SweeperInput{
		Input: inpExclusive,
		params: Params{
			Budget:         budgetHigh,
			ExclusiveGroup: &exclusiveGroup,
		},
		DeadlineHeight: deadline1,
	}

	// For each deadline height, create two inputs with different budgets,
	// one below the min fee rate and one above it. We should see the lower
	// one being filtered out.
	for i, deadline := range []int32{
		defaultDeadline, deadline1, deadline2,
	} {
		// Define three outpoints.
		opLow := wire.OutPoint{
			Hash:  chainhash.Hash{byte(i)},
			Index: uint32(i),
		}
		opHigh1 := wire.OutPoint{
			Hash:  chainhash.Hash{byte(i + 1000)},
			Index: uint32(i + 1000),
		}
		opHigh2 := wire.OutPoint{
			Hash:  chainhash.Hash{byte(i + 2000)},
			Index: uint32(i + 2000),
		}

		// Create mock inputs.
		inpLow := &input.MockInput{}
		defer inpLow.AssertExpectations(t)

		inpHigh1 := &input.MockInput{}
		defer inpHigh1.AssertExpectations(t)

		inpHigh2 := &input.MockInput{}
		defer inpHigh2.AssertExpectations(t)

		// Mock the `OutPoint` method to return the unique outpoint.
		//
		// We expect the low budget input to call this method once in
		// `filterInputs`.
		inpLow.On("OutPoint").Return(opLow).Once()

		// The number of times this method is called is dependent on
		// the log level.
		inpHigh1.On("OutPoint").Return(opHigh1).Maybe()
		inpHigh2.On("OutPoint").Return(opHigh2).Maybe()

		// Mock the `WitnessType` method to return the witness type.
		inpLow.On("WitnessType").Return(wt)
		inpHigh1.On("WitnessType").Return(wt)
		inpHigh2.On("WitnessType").Return(wt)

		// Mock the `RequiredTxOut` to return nil.
		inpHigh1.On("RequiredTxOut").Return(nil)
		inpHigh2.On("RequiredTxOut").Return(nil)

		// Mock the `RequiredLockTime` to return 0.
		inpHigh1.On("RequiredLockTime").Return(uint32(0), false)
		inpHigh2.On("RequiredLockTime").Return(uint32(0), false)

		// Add the low input, which should be filtered out.
		inputs[opLow] = &SweeperInput{
			Input: inpLow,
			params: Params{
				Budget: budgetLow,
			},
			DeadlineHeight: deadline,
		}

		// Add the high inputs, which should be included.
		inputs[opHigh1] = &SweeperInput{
			Input: inpHigh1,
			params: Params{
				Budget: budgetHigh,
			},
			DeadlineHeight: deadline,
		}
		inputs[opHigh2] = &SweeperInput{
			Input: inpHigh2,
			params: Params{
				Budget: budgetHigh,
			},
			DeadlineHeight: deadline,
		}
	}

	// Create a budget aggregator with a max number of inputs set to 100.
	b := NewBudgetAggregator(estimator, DefaultMaxInputsPerTx)

	// Call the method under test.
	result := b.ClusterInputs(inputs)

	// We expect four input sets to be returned, one for each deadline and
	// extra one for the exclusive input.
	require.Len(t, result, 4)

	// The last set should be the exclusive input that has only one input.
	setExclusive := result[3]
	require.Len(t, setExclusive.Inputs(), 1)

	// Check the each of rest has exactly two inputs.
	deadlines := make(map[int32]struct{})
	for _, set := range result[:3] {
		// We expect two inputs in each set.
		require.Len(t, set.Inputs(), 2)

		// We expect each set to have the expected budget.
		require.Equal(t, budgetHigh*2, set.Budget())

		// Save the deadlines.
		deadlines[set.DeadlineHeight()] = struct{}{}
	}

	// We expect to see all three deadlines.
	require.Contains(t, deadlines, defaultDeadline)
	require.Contains(t, deadlines, deadline1)
	require.Contains(t, deadlines, deadline2)
}

// TestSplitOnLocktime asserts `splitOnLocktime` works as expected.
func TestSplitOnLocktime(t *testing.T) {
	t.Parallel()

	// Create two locktimes.
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
	// Create cluster three, which has no locktime.
	input5NoLockTime := &input.MockInput{}
	input6NoLockTime := &input.MockInput{}
	input5NoLockTime.On("RequiredLockTime").Return(uint32(0), false)
	input6NoLockTime.On("RequiredLockTime").Return(uint32(0), false)

	// Mock `OutPoint` - it may or may not be called due to log settings.
	input1LockTime1.On("OutPoint").Return(wire.OutPoint{Index: 1}).Maybe()
	input2LockTime1.On("OutPoint").Return(wire.OutPoint{Index: 2}).Maybe()
	input3LockTime2.On("OutPoint").Return(wire.OutPoint{Index: 3}).Maybe()
	input4LockTime2.On("OutPoint").Return(wire.OutPoint{Index: 4}).Maybe()
	input5NoLockTime.On("OutPoint").Return(wire.OutPoint{Index: 5}).Maybe()
	input6NoLockTime.On("OutPoint").Return(wire.OutPoint{Index: 6}).Maybe()

	// With the inner Input being mocked, we can now create the pending
	// inputs.
	input1 := SweeperInput{Input: input1LockTime1}
	input2 := SweeperInput{Input: input2LockTime1}
	input3 := SweeperInput{Input: input3LockTime2}
	input4 := SweeperInput{Input: input4LockTime2}
	input5 := SweeperInput{Input: input5NoLockTime}
	input6 := SweeperInput{Input: input6NoLockTime}

	// Call the method under test.
	inputs := []SweeperInput{input1, input2, input3, input4, input5, input6}
	result := splitOnLocktime(inputs)

	// We expect the no locktime inputs to be grouped with locktime2.
	expectedResult := map[uint32][]SweeperInput{
		lockTime1: {input1, input2},
		lockTime2: {input3, input4, input5, input6},
	}
	require.Len(t, result[lockTime1], 2)
	require.Len(t, result[lockTime2], 4)
	require.Equal(t, expectedResult, result)

	// Test the case where there are no locktime inputs.
	inputs = []SweeperInput{input5, input6}
	result = splitOnLocktime(inputs)

	// We expect the no locktime inputs to be returned as is.
	expectedResult = map[uint32][]SweeperInput{
		uint32(0): {input5, input6},
	}
	require.Len(t, result[uint32(0)], 2)
	require.Equal(t, expectedResult, result)
}
