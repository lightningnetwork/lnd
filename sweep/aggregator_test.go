package sweep

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var (
	testHeight = int32(800000)
)

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
	wtErr.On("SizeUpperBound").Return(
		lntypes.WeightUnit(0), false, dummyErr).Once()

	// Create a mock WitnessType that gives the size.
	wt := &input.MockWitnessType{}
	defer wt.AssertExpectations(t)

	// Mock the `SizeUpperBound` method to return the size four times.
	const wu lntypes.WeightUnit = 100
	wt.On("SizeUpperBound").Return(wu, true, nil).Times(4)

	// Calculate the input size.
	inpSize := lntypes.VByte(input.InputSize).ToWU() + wu

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
		budgetLow   = minFeeRate.FeeForWeight(inpSize) - 1
		budgetEqual = minFeeRate.FeeForWeight(inpSize)
		budgetHigh  = minFeeRate.FeeForWeight(inpSize) + 1

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
	b := NewBudgetAggregator(estimator, 0, fn.None[AuxSweeper]())

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
	b := NewBudgetAggregator(nil, 0, fn.None[AuxSweeper]())

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
	b := NewBudgetAggregator(nil, 2, fn.None[AuxSweeper]())

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
	const wu lntypes.WeightUnit = 100
	wt.On("SizeUpperBound").Return(wu, true, nil).Times(10)

	// Calculate the input size.
	inpSize := lntypes.VByte(input.InputSize).ToWU() + wu

	wt.On("String").Return("mock witness type")

	// Mock the estimator to return a constant fee rate.
	const minFeeRate = chainfee.SatPerKWeight(1000)
	estimator.On("RelayFeePerKW").Return(minFeeRate).Once()

	var (
		// Define two budget values, one below the min fee rate and one
		// above it.
		budgetLow  = minFeeRate.FeeForWeight(inpSize) - 1
		budgetHigh = minFeeRate.FeeForWeight(inpSize) + 1

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
	b := NewBudgetAggregator(
		estimator, DefaultMaxInputsPerTx, fn.None[AuxSweeper](),
	)

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
