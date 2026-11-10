package sweep

import (
	"errors"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// createP2WKHInput returns a P2WKH test input with the specified amount.
func createP2WKHInput(amt btcutil.Amount) input.Input {
	input := createTestInput(int64(amt), input.WitnessKeyHash)
	return &input
}

// TestNewBudgetInputSet checks `NewBudgetInputSet` correctly validates the
// supplied inputs and returns the error.
func TestNewBudgetInputSet(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Pass an empty slice and expect an error.
	set, err := NewBudgetInputSet(
		[]SweeperInput{}, testHeight, fn.None[AuxSweeper](),
	)
	rt.ErrorContains(err, "inputs slice is empty")
	rt.Nil(set)

	// Create two inputs with different deadline heights.
	inp0 := createP2WKHInput(1000)
	inp1 := createP2WKHInput(1000)
	inp2 := createP2WKHInput(1000)
	input0 := SweeperInput{
		Input: inp0,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.None[int32](),
		},
	}
	input1 := SweeperInput{
		Input: inp1,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.Some(int32(1)),
		},
	}
	input2 := SweeperInput{
		Input: inp2,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.Some(int32(2)),
		},
	}
	input3 := SweeperInput{
		Input: inp2,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.Some(testHeight),
		},
	}

	// Pass a slice of inputs with different deadline heights.
	set, err = NewBudgetInputSet(
		[]SweeperInput{input1, input2}, testHeight,
		fn.None[AuxSweeper](),
	)
	rt.ErrorContains(err, "input deadline height not matched")
	rt.Nil(set)

	// Pass a slice of inputs that only one input has the deadline height,
	// but it has a different value than the specified testHeight.
	set, err = NewBudgetInputSet(
		[]SweeperInput{input0, input2}, testHeight,
		fn.None[AuxSweeper](),
	)
	rt.ErrorContains(err, "input deadline height not matched")
	rt.Nil(set)

	// Pass a slice of inputs that are duplicates.
	set, err = NewBudgetInputSet(
		[]SweeperInput{input3, input3}, testHeight,
		fn.None[AuxSweeper](),
	)
	rt.ErrorContains(err, "duplicate inputs")
	rt.Nil(set)

	// Pass a slice of inputs that only one input has the deadline height.
	set, err = NewBudgetInputSet(
		[]SweeperInput{input0, input3}, testHeight,
		fn.None[AuxSweeper](),
	)
	rt.NoError(err)
	rt.NotNil(set)
}

// TestBudgetInputSetAddInput checks that `addInput` correctly updates the
// budget of the input set.
func TestBudgetInputSetAddInput(t *testing.T) {
	t.Parallel()

	// Create a testing input with a budget of 100 satoshis.
	input := createP2WKHInput(1000)
	pi := &SweeperInput{
		Input: input,
		params: Params{
			Budget: 100,
		},
	}

	// Initialize an input set, which adds the above input.
	set, err := NewBudgetInputSet(
		[]SweeperInput{*pi}, testHeight, fn.None[AuxSweeper](),
	)
	require.NoError(t, err)

	// Add the input to the set again.
	set.addInput(*pi)

	// The set should now have two inputs.
	require.Len(t, set.inputs, 2)
	require.Equal(t, pi, set.inputs[0])
	require.Equal(t, pi, set.inputs[1])

	// The set should have a budget of 200 satoshis.
	require.Equal(t, btcutil.Amount(200), set.Budget())
}

// TestAddWalletInput asserts `addWalletInput` successfully converts a wallet
// UTXO into a `SweeperInput` with the correct deadline.
func TestAddWalletInput(t *testing.T) {
	t.Parallel()

	// Create a testing deadline.
	deadline := int32(1000)

	// Initialize an empty input set.
	set := &BudgetInputSet{
		deadlineHeight: deadline,
	}

	// Create an utxo with unknown address type to trigger an error.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.UnknownAddressType,
	}

	// Check that the error is returned from addWalletInput.
	err := set.addWalletInput(utxo)
	require.Error(t, err)

	// Create a wallet utxo.
	utxo = &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       1000,
	}

	// Check that no error is returned from addWalletInput.
	err = set.addWalletInput(utxo)
	require.NoError(t, err)

	// Check the input has been added to the set.
	require.Len(t, set.inputs, 1)

	// Assert the wallet input is added using the set's deadline.
	inp := set.inputs[0]
	require.True(t, inp.params.DeadlineHeight.IsSome())
	require.Equal(t, deadline, inp.params.DeadlineHeight.UnsafeFromSome())
}

// TestNeedWalletInput checks that NeedWalletInput correctly determines if a
// wallet input is needed.
func TestNeedWalletInput(t *testing.T) {
	t.Parallel()

	// Create a mock input that doesn't have required outputs.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(nil)
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})
	defer mockInput.AssertExpectations(t)

	// Create a mock input that has required outputs.
	mockInputRequireOutput := &input.MockInput{}
	mockInputRequireOutput.On("RequiredTxOut").Return(&wire.TxOut{})
	mockInputRequireOutput.On("OutPoint").Return(
		wire.OutPoint{Hash: chainhash.Hash{2}},
	)
	defer mockInputRequireOutput.AssertExpectations(t)

	// We now create two pending inputs each has a budget of 100 satoshis.
	const budget = 100

	// Create the pending input that doesn't have a required output.
	piBudget := SweeperInput{
		Input:  mockInput,
		params: Params{Budget: budget},
	}

	// Create the pending input that has a required output.
	piRequireOutput := SweeperInput{
		Input:  mockInputRequireOutput,
		params: Params{Budget: budget},
	}

	testCases := []struct {
		name        string
		setupInputs func() []SweeperInput
		extraBudget btcutil.Amount
		need        bool
		err         error
	}{
		{
			// When there are no pending inputs, we won't need a
			// wallet input. Technically this is be an invalid
			// state.
			name: "no inputs",
			setupInputs: func() []SweeperInput {
				return nil
			},
			need: false,
			err:  errEmptyInputs,
		},
		{
			// When there's no required output, we don't need a
			// wallet input.
			name: "no required outputs",
			setupInputs: func() []SweeperInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget,
					},
				}
				mockInput.On("SignDesc").Return(sd).Once()

				return []SweeperInput{piBudget}
			},
			need: false,
		},
		{
			// When there's no required normal outputs, but an extra
			// budget from custom channels, we will need a wallet
			// input.
			name: "no required normal outputs but extra budget",
			setupInputs: func() []SweeperInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget,
					},
				}
				mockInput.On("SignDesc").Return(sd).Once()

				return []SweeperInput{piBudget}
			},
			extraBudget: 1000,
			need:        true,
		},
		{
			// When the output value cannot cover the budget, we
			// need a wallet input.
			name: "output value cannot cover budget",
			setupInputs: func() []SweeperInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget - 1,
					},
				}

				mockInput.On("SignDesc").Return(sd).Once()
				// These two methods are only invoked when the
				// unit test is running with a logger.
				mockInput.On("OutPoint").Return(
					wire.OutPoint{Hash: chainhash.Hash{1}},
				).Maybe()
				mockInput.On("WitnessType").Return(
					input.CommitmentAnchor,
				).Maybe()

				return []SweeperInput{piBudget}
			},
			need: true,
		},
		{
			// When there's only inputs that require outputs, we
			// need wallet inputs.
			name: "only required outputs",
			setupInputs: func() []SweeperInput {
				return []SweeperInput{piRequireOutput}
			},
			need: true,
		},
		{
			// When there's a mix of inputs, but the borrowable
			// budget cannot cover the required, we need a wallet
			// input.
			name: "not enough budget to be borrowed",
			setupInputs: func() []SweeperInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				//
				// NOTE: the value is exactly the same as the
				// budget so we can't borrow any more.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget,
					},
				}
				mockInput.On("SignDesc").Return(sd).Once()

				return []SweeperInput{
					piBudget, piRequireOutput,
				}
			},
			need: true,
		},
		{
			// When there's a mix of inputs, and the budget can be
			// borrowed covers the required, we don't need wallet
			// inputs.
			name: "enough budget to be borrowed",
			setupInputs: func() []SweeperInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				//
				// NOTE: the value is exactly the same as the
				// budget so we can't borrow any more.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget * 2,
					},
				}
				mockInput.On("SignDesc").Return(sd).Once()
				piBudget.Input = mockInput

				return []SweeperInput{
					piBudget, piRequireOutput,
				}
			},
			need: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup testing inputs.
			inputs := tc.setupInputs()

			// If an extra budget is set, then we'll update the mock
			// to expect the extra budget.
			mockAuxSweeper := &MockAuxSweeper{}
			mockAuxSweeper.On("ExtraBudgetForInputs").Return(
				fn.Ok(tc.extraBudget),
			)

			// Initialize an input set, which adds the testing
			// inputs.
			set, err := NewBudgetInputSet(
				inputs, 0, fn.Some[AuxSweeper](mockAuxSweeper),
			)
			if err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}

			result := set.NeedWalletInput()

			require.Equal(t, tc.need, result)
			mockAuxSweeper.AssertExpectations(t)
		})
	}
}

// TestAddWalletInputsReturnErr tests the three possible errors returned from
// AddWalletInputs:
// - error from ListUnspentWitnessFromDefaultAccount.
// - error from createWalletTxInput.
// - error when wallet doesn't have utxos.
func TestAddWalletInputsReturnErr(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Initialize an empty input set.
	set := &BudgetInputSet{}

	// Specify the min and max confs used in
	// ListUnspentWitnessFromDefaultAccount.
	min, max := int32(1), int32(math.MaxInt32)

	// Mock the wallet to return an error.
	dummyErr := errors.New("dummy error")
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return(nil, dummyErr).Once()

	// Check that the error is returned from
	// ListUnspentWitnessFromDefaultAccount.
	err := set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.ErrorIs(t, err, dummyErr)

	// Create an utxo with unknown address type to trigger an error.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.UnknownAddressType,
	}

	// Mock the wallet to return the above utxo.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{utxo}, nil).Once()

	// Check that the error is returned from createWalletTxInput.
	err = set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.Error(t, err)

	// Mock the wallet to return empty utxos.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{}, nil).Once()

	// Check that the error is returned from not having wallet inputs.
	err = set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.ErrorIs(t, err, ErrNotEnoughInputs)
}

// TestAddWalletInputsNotEnoughInputs checks that when there are not enough
// wallet utxos, no error is returned as long as the wallet is not empty.
func TestAddWalletInputsNotEnoughInputs(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Specify the min and max confs used in
	// ListUnspentWitnessFromDefaultAccount.
	min, max := int32(1), int32(math.MaxInt32)

	// Assume the desired budget is 10k satoshis.
	const budget = 10_000

	// Create a mock input that has required outputs.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(&wire.TxOut{})

	// Mock methods used when weighting the input.
	mockInput.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput.On("UnconfParent").Return(nil).Maybe()
	defer mockInput.AssertExpectations(t)

	sd := &input.SignDescriptor{
		Output: &wire.TxOut{
			Value: budget,
		},
	}
	mockInput.On("SignDesc").Return(sd).Once()

	// Create a pending input that requires 10k satoshis.
	pi := &SweeperInput{
		Input:  mockInput,
		params: Params{Budget: budget},
	}

	// Create a wallet utxo that cannot cover the budget.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       budget - 1,
	}

	// Mock the wallet to return the above utxo.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{utxo}, nil).Once()

	// Initialize an input set with the pending input.
	set := BudgetInputSet{inputs: []*SweeperInput{pi}}

	// Add wallet inputs to the input set, which should return no error
	// although the wallet cannot cover the budget.
	err := set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.NoError(t, err)

	// Check that the budget set is updated.
	require.Len(t, set.inputs, 2)
}

// TestAddWalletInputsEmptyWalletSuccess checks that when the wallet is empty,
// if there is a normal input, no error is returned.
func TestAddWalletInputsEmptyWalletSuccess(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Specify the min and max confs used in
	// ListUnspentWitnessFromDefaultAccount.
	minConf, maxConf := int32(1), int32(math.MaxInt32)

	// Assume the desired budget is 10k satoshis.
	const budget = 10_000

	// Create a mock input that has required outputs.
	mockInput1 := &input.MockInput{}
	defer mockInput1.AssertExpectations(t)

	mockInput1.On("RequiredTxOut").Return(&wire.TxOut{})

	// Mock methods used when weighting the input.
	mockInput1.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput1.On("UnconfParent").Return(nil).Maybe()

	sd := &input.SignDescriptor{
		Output: &wire.TxOut{
			Value: budget,
		},
	}
	mockInput1.On("SignDesc").Return(sd).Once()

	// Create a pending input that requires 10k satoshis.
	pi1 := &SweeperInput{
		Input:  mockInput1,
		params: Params{Budget: budget},
	}

	// Create a mock input that doesn't require outputs.
	mockInput2 := &input.MockInput{}
	defer mockInput2.AssertExpectations(t)

	mockInput2.On("RequiredTxOut").Return(nil)

	// Mock methods used when weighting the input.
	mockInput2.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput2.On("UnconfParent").Return(nil).Maybe()

	sd2 := &input.SignDescriptor{
		Output: &wire.TxOut{
			Value: budget,
		},
	}
	mockInput2.On("SignDesc").Return(sd2).Once()

	// Create a pending input that requires 10k satoshis.
	pi2 := &SweeperInput{
		Input:  mockInput2,
		params: Params{Budget: budget},
	}

	// Mock the wallet to return empty utxos.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		minConf, maxConf).Return([]*lnwallet.Utxo{}, nil).Once()

	// Initialize an input set with the pending inputs.
	set := BudgetInputSet{inputs: []*SweeperInput{pi1, pi2}}

	// Add wallet inputs to the input set, which should return no error
	// although the wallet is empty.
	err := set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.NoError(t, err)
}

// TestAddWalletInputsLargestFirst checks that wallet inputs are selected
// largest first so the fewest inputs cover the budget.
func TestAddWalletInputsLargestFirst(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const (
		budget  = 10_000
		numDust = 100
	)

	// Create an input whose required output prevents it from paying its own
	// budget.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(&wire.TxOut{})

	// Mock methods used when weighting the input.
	mockInput.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput.On("UnconfParent").Return(nil).Maybe()
	defer mockInput.AssertExpectations(t)

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input: mockInput,
			params: Params{
				Budget: budget,
			},
		}},
	}

	// Add many dust UTXOs before one UTXO that can cover the full budget.
	utxos := make([]*lnwallet.Utxo, 0, numDust+1)
	for range numDust {
		utxos = append(utxos, &lnwallet.Utxo{
			AddressType: lnwallet.WitnessPubKey,
			Value:       330,
		})
	}
	utxos = append(utxos, &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       budget,
	})

	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(utxos, nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))
	require.Equal(t, 2, len(set.inputs))
	require.EqualValues(
		t, budget, set.inputs[1].SignDesc().Output.Value,
	)
}

// TestAddWalletInputsManySmallUtxos checks that a wallet holding only small
// UTXOs funds the full budget, however many inputs that takes, as long as the
// sweep stays below the standardness weight limit. Stopping at a fixed input
// count would leave the set short of the budget the fee bumper targets.
func TestAddWalletInputsManySmallUtxos(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const (
		budget    = 200_000
		numUtxos  = 200
		utxoValue = 1_000
	)

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input:  newRequiredOutputInput(t, 400_000),
			params: Params{Budget: budget},
		}},
	}

	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(newP2WKHUtxos(numUtxos, utxoValue, 0), nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// Every UTXO is needed to cover the budget, so all of them are added
	// and the set no longer needs wallet inputs.
	require.Len(t, set.inputs, 1+numUtxos)
	require.False(t, set.NeedWalletInput())
}

// TestAddWalletInputsWeightLimit checks that when a wallet holds many small
// UTXOs that cannot cover the budget, wallet inputs are only added while the
// sweep transaction stays below the standardness weight limit.
func TestAddWalletInputsWeightLimit(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const (
		budget    = 10_000_000
		numUtxos  = 2_000
		utxoValue = 1_000
	)

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input:  newRequiredOutputInput(t, budget),
			params: Params{Budget: budget},
		}},
	}

	// Each UTXO pays for its own weight at the relay fee rate, but all
	// of them together would make the sweep exceed the weight limit.
	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(newP2WKHUtxos(numUtxos, utxoValue, 0), nil).Once()

	// The set is still swept with the inputs that fit, as at least one of
	// them can pay fees.
	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// Some wallet inputs are added, but the weight limit stops the
	// selection before the wallet is exhausted.
	numWallet := len(set.inputs) - 1
	require.Positive(t, numWallet)
	require.Less(t, numWallet, numUtxos)
	require.True(t, set.NeedWalletInput())

	// Recompute the weight of the final input set and assert it stays
	// within the standardness weight limit, leaving the reserved margin,
	// and that one more wallet input would not have fit.
	weightEstimate := newWeightEstimator(chainfee.FeePerKwFloor, 0)
	for _, inp := range set.inputs {
		require.NoError(t, weightEstimate.add(inp.Input))

		if txOut := inp.RequiredTxOut(); txOut != nil {
			weightEstimate.addOutput(txOut)
		}
	}
	weightEstimate.addP2TROutput()

	maxWeight := maxStandardTxWeight - sweepTxWeightMargin
	require.LessOrEqual(t, weightEstimate.weight(), maxWeight)

	require.NoError(t, weightEstimate.add(set.inputs[1].Input))
	require.Greater(t, weightEstimate.weight(), maxWeight)
}

// TestAddWalletInputsSmallUtxosOnly checks that small wallet UTXOs are added
// until they cover the set's budget.
func TestAddWalletInputsSmallUtxosOnly(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const (
		budget    = 500_000
		numUtxos  = 20
		utxoValue = 50_000
	)

	// Create an input whose required output prevents it from paying its
	// own budget, so it cannot pay fees itself.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(&wire.TxOut{})
	mockInput.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput.On("UnconfParent").Return(nil).Maybe()
	defer mockInput.AssertExpectations(t)

	sd := &input.SignDescriptor{
		Output: &wire.TxOut{
			Value: budget,
		},
	}
	mockInput.On("SignDesc").Return(sd).Maybe()

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input: mockInput,
			params: Params{
				Budget: budget,
			},
		}},
	}

	// Create enough small UTXOs to cover the budget.
	utxos := make([]*lnwallet.Utxo, 0, numUtxos)
	for i := range numUtxos {
		utxos = append(utxos, &lnwallet.Utxo{
			AddressType: lnwallet.WitnessPubKey,
			Value:       utxoValue,
			OutPoint:    wire.OutPoint{Index: uint32(i)},
		})
	}

	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(utxos, nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// The budget of 500_000 needs ten UTXOs of 50_000.
	require.Len(t, set.inputs, 1+budget/utxoValue)
	require.True(t, set.hasNormalInput())
	require.False(t, set.NeedWalletInput())
}

// TestAddWalletInputsRelayFeeBoundary checks that a wallet UTXO is only added
// if its value is greater than the fee for its input weight at the relay fee
// rate.
func TestAddWalletInputsRelayFeeBoundary(t *testing.T) {
	t.Parallel()

	const relayFeeRate = chainfee.SatPerKWeight(10_000)

	// Calculate the weight added by a P2WKH wallet input to the test set.
	var estimator input.TxWeightEstimator
	require.NoError(t,
		input.WitnessKeyHash.AddWeightEstimation(&estimator))
	estimator.AddP2TROutput()
	baseWeight := estimator.Weight()
	require.NoError(t,
		input.WitnessKeyHash.AddWeightEstimation(&estimator))
	walletInputFee := relayFeeRate.FeeForWeight(
		estimator.Weight() - baseWeight,
	)

	testCases := []struct {
		name        string
		value       btcutil.Amount
		shouldAdd   bool
		expectedErr error
	}{
		{
			name:        "one sat below",
			value:       walletInputFee - 1,
			expectedErr: ErrNotEnoughInputs,
		},
		{
			name:        "equal",
			value:       walletInputFee,
			expectedErr: ErrNotEnoughInputs,
		},
		{
			name:      "one sat above",
			value:     walletInputFee + 1,
			shouldAdd: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wallet := &MockWallet{}
			defer wallet.AssertExpectations(t)

			mockInput := &input.MockInput{}
			mockInput.On("RequiredTxOut").Return(&wire.TxOut{})
			mockInput.On("WitnessType").Return(
				input.WitnessKeyHash,
			)
			defer mockInput.AssertExpectations(t)

			budget := walletInputFee + 1
			set := BudgetInputSet{
				inputs: []*SweeperInput{{
					Input: mockInput,
					params: Params{
						Budget: budget,
					},
				}},
			}

			utxo := &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       tc.value,
			}
			wallet.On(
				"ListUnspentWitnessFromDefaultAccount",
				int32(1), int32(math.MaxInt32),
			).Return([]*lnwallet.Utxo{utxo}, nil).Once()

			err := set.AddWalletInputs(wallet, relayFeeRate)
			require.ErrorIs(t, err, tc.expectedErr)
			if tc.shouldAdd {
				require.Len(t, set.inputs, 2)
				require.False(t, set.NeedWalletInput())

				return
			}

			require.Len(t, set.inputs, 1)
		})
	}
}

// TestAddWalletInputsSuccess checks that when there are enough wallet utxos,
// they are added to the input set.
func TestAddWalletInputsSuccess(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Specify the min and max confs used in
	// ListUnspentWitnessFromDefaultAccount.
	min, max := int32(1), int32(math.MaxInt32)

	// Assume the desired budget is 10k satoshis.
	const budget = 10_000

	// Create a mock input that has required outputs.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(&wire.TxOut{})
	defer mockInput.AssertExpectations(t)

	// Create a pending input that requires 10k satoshis.
	deadline := int32(1000)
	pi := &SweeperInput{
		Input: mockInput,
		params: Params{
			Budget:         budget,
			DeadlineHeight: fn.Some(deadline),
		},
	}

	// Mock methods used in loggings.
	//
	// NOTE: these methods are not functional as they are only used for
	// loggings in debug or trace mode so we use arbitrary values.
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})
	mockInput.On("WitnessType").Return(input.CommitmentAnchor)

	// Mock the method used when checking for CPFP requirements.
	mockInput.On("UnconfParent").Return(nil).Maybe()

	// Create a wallet utxo that cannot cover the budget.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       budget - 1,
	}

	// Mock the wallet to return the two utxos which can cover the budget.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{utxo, utxo}, nil).Once()

	// Initialize an input set with the pending input.
	set, err := NewBudgetInputSet(
		[]SweeperInput{*pi}, deadline, fn.None[AuxSweeper](),
	)
	require.NoError(t, err)

	// Add wallet inputs to the input set.
	err = set.AddWalletInputs(wallet, chainfee.FeePerKwFloor)
	require.NoError(t, err)

	// Check that the budget set is updated.
	require.Len(t, set.inputs, 3)

	// The first input is the pending input.
	require.Equal(t, pi, set.inputs[0])

	// The second and third inputs are wallet inputs that have
	// DeadlineHeight set.
	input2Deadline := set.inputs[1].params.DeadlineHeight
	require.Equal(t, deadline, input2Deadline.UnsafeFromSome())
	input3Deadline := set.inputs[2].params.DeadlineHeight
	require.Equal(t, deadline, input3Deadline.UnsafeFromSome())

	// Finally, check the interface methods.
	require.EqualValues(t, budget, set.Budget())
	require.Equal(t, deadline, set.DeadlineHeight())
	// Weak check, a strong check is to open the slice and check each item.
	require.Len(t, set.inputs, 3)
}

// newRequiredOutputInput returns a mock input with a required output, such as
// a second-level HTLC input, which cannot pay fees itself.
func newRequiredOutputInput(t *testing.T,
	value btcutil.Amount) *input.MockInput {

	mockInput := &input.MockInput{}
	t.Cleanup(func() { mockInput.AssertExpectations(t) })

	mockInput.On("RequiredTxOut").Return(&wire.TxOut{})
	mockInput.On("WitnessType").Return(input.WitnessKeyHash)
	mockInput.On("UnconfParent").Return(nil).Maybe()
	mockInput.On("SignDesc").Return(&input.SignDescriptor{
		Output: &wire.TxOut{Value: int64(value)},
	}).Maybe()

	return mockInput
}

// newP2WKHUtxos returns num P2WKH wallet UTXOs of the given value, with output
// indexes starting at firstIndex.
func newP2WKHUtxos(num int, value btcutil.Amount,
	firstIndex uint32) []*lnwallet.Utxo {

	utxos := make([]*lnwallet.Utxo, 0, num)
	for i := range num {
		utxos = append(utxos, &lnwallet.Utxo{
			AddressType: lnwallet.WitnessPubKey,
			Value:       value,
			OutPoint: wire.OutPoint{
				Index: firstIndex + uint32(i),
			},
		})
	}

	return utxos
}

// TestAddWalletInputsAnchorSmallUtxos checks that an anchor CPFP set, whose
// anchor is far too small to cover its budget, is funded by wallet UTXOs.
func TestAddWalletInputsAnchorSmallUtxos(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const (
		budget    = 500_000
		numUtxos  = 20
		utxoValue = 50_000
	)

	// Create an anchor input. It has no required output, but its value
	// is far below its budget, which must be borrowed from the wallet.
	anchor := &input.MockInput{}
	defer anchor.AssertExpectations(t)

	anchor.On("RequiredTxOut").Return(nil)
	anchor.On("WitnessType").Return(input.CommitmentAnchor)
	anchor.On("UnconfParent").Return(nil).Maybe()
	anchor.On("SignDesc").Return(&input.SignDescriptor{
		Output: &wire.TxOut{Value: 330},
	})

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input:  anchor,
			params: Params{Budget: budget},
		}},
	}

	// Return enough small UTXOs to cover the anchor's budget.
	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(newP2WKHUtxos(numUtxos, utxoValue, 0), nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// The anchor plus ten UTXOs of 50_000 cover the budget of 500_000.
	require.Len(t, set.inputs, 1+budget/utxoValue)
	require.False(t, set.NeedWalletInput())
}

// TestAddWalletInputsTopUpAfterLargeUtxo checks that when the largest wallet
// UTXO cannot cover the budget, smaller UTXOs are added on top of it.
func TestAddWalletInputsTopUpAfterLargeUtxo(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const budget = 500_000

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input:  newRequiredOutputInput(t, budget),
			params: Params{Budget: budget},
		}},
	}

	// The 100_000 sat UTXO is considered before the 50_000 sat UTXOs.
	utxos := newP2WKHUtxos(20, 50_000, 1)
	utxos = append(utxos, &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       100_000,
	})

	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(utxos, nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// The large UTXO is added first, then eight UTXOs of 50_000 top up
	// the budget.
	require.Len(t, set.inputs, 1+1+8)
	require.EqualValues(
		t, 100_000, set.inputs[1].SignDesc().Output.Value,
	)
	require.False(t, set.NeedWalletInput())
}

// TestAddWalletInputsValueDensityOrder checks that wallet UTXOs are
// considered in order of value per weight unit, so a smaller UTXO of a
// lighter address type is preferred over a larger, heavier one when it pays
// more per weight unit.
func TestAddWalletInputsValueDensityOrder(t *testing.T) {
	t.Parallel()

	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	const budget = 9_000

	set := BudgetInputSet{
		inputs: []*SweeperInput{{
			Input:  newRequiredOutputInput(t, budget),
			params: Params{Budget: budget},
		}},
	}

	// A P2WKH input adds 273 weight units and a P2TR key spend input
	// fewer, so the 9_500 sat P2TR UTXO pays more per weight unit than
	// the 10_000 sat P2WKH UTXO.
	utxos := []*lnwallet.Utxo{
		{
			AddressType: lnwallet.WitnessPubKey,
			Value:       10_000,
			OutPoint:    wire.OutPoint{Index: 0},
		},
		{
			AddressType: lnwallet.TaprootPubkey,
			Value:       9_500,
			OutPoint:    wire.OutPoint{Index: 1},
		},
	}

	wallet.On(
		"ListUnspentWitnessFromDefaultAccount",
		int32(1), int32(math.MaxInt32),
	).Return(utxos, nil).Once()

	require.NoError(t, set.AddWalletInputs(wallet, chainfee.FeePerKwFloor))

	// The P2TR UTXO alone covers the budget.
	require.Len(t, set.inputs, 2)
	require.EqualValues(t, 9_500, set.inputs[1].SignDesc().Output.Value)
}

// TestAddWalletInputsExactDensityOrder checks that wallet UTXOs are ordered by
// their exact value per weight unit. Two UTXOs whose densities round to the
// same fee rate but fall on opposite sides of the relay fee rate must not stop
// selection before the one that pays for its own weight is considered,
// whatever order the wallet returns them in.
func TestAddWalletInputsExactDensityOrder(t *testing.T) {
	t.Parallel()

	const relayFeeRate = chainfee.SatPerKWeight(1_502)

	// A P2WKH UTXO of 410 sat has a density just below the relay fee
	// rate, and a P2TR UTXO of 347 sat one just above it. Both round to
	// the relay fee rate.
	p2wkh := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       410,
		OutPoint:    wire.OutPoint{Index: 0},
	}
	p2tr := &lnwallet.Utxo{
		AddressType: lnwallet.TaprootPubkey,
		Value:       347,
		OutPoint:    wire.OutPoint{Index: 1},
	}

	// Check the premise of the test against the weight each UTXO adds to
	// a set with one P2WKH input and a P2TR output.
	addedWeight := func(wt input.WitnessType) lntypes.WeightUnit {
		var estimator input.TxWeightEstimator
		require.NoError(t, input.WitnessKeyHash.AddWeightEstimation(
			&estimator,
		))
		estimator.AddP2TROutput()
		base := estimator.Weight()
		require.NoError(t, wt.AddWeightEstimation(&estimator))

		return estimator.Weight() - base
	}
	p2wkhWeight := addedWeight(input.WitnessKeyHash)
	p2trWeight := addedWeight(input.TaprootPubKeySpend)

	require.LessOrEqual(t, p2wkh.Value,
		relayFeeRate.FeeForWeight(p2wkhWeight))
	require.Greater(t, p2tr.Value, relayFeeRate.FeeForWeight(p2trWeight))
	require.Equal(t,
		chainfee.NewSatPerKWeight(p2wkh.Value, p2wkhWeight),
		chainfee.NewSatPerKWeight(p2tr.Value, p2trWeight),
	)

	testCases := []struct {
		name  string
		utxos []*lnwallet.Utxo
	}{
		{
			name:  "p2wkh first",
			utxos: []*lnwallet.Utxo{p2wkh, p2tr},
		},
		{
			name:  "p2tr first",
			utxos: []*lnwallet.Utxo{p2tr, p2wkh},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wallet := &MockWallet{}
			defer wallet.AssertExpectations(t)

			const budget = 10_000
			set := BudgetInputSet{
				inputs: []*SweeperInput{{
					Input: newRequiredOutputInput(
						t, budget,
					),
					params: Params{Budget: budget},
				}},
			}

			wallet.On(
				"ListUnspentWitnessFromDefaultAccount",
				int32(1), int32(math.MaxInt32),
			).Return(tc.utxos, nil).Once()

			require.NoError(t,
				set.AddWalletInputs(wallet, relayFeeRate))

			// Only the P2TR UTXO pays for its own weight, so it is
			// the only wallet input added.
			require.Len(t, set.inputs, 2)
			require.EqualValues(t, p2tr.Value,
				set.inputs[1].SignDesc().Output.Value)
		})
	}
}
