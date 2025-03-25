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
	"github.com/lightningnetwork/lnd/lnwallet"
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
	err := set.AddWalletInputs(wallet)
	require.ErrorIs(t, err, dummyErr)

	// Create an utxo with unknown address type to trigger an error.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.UnknownAddressType,
	}

	// Mock the wallet to return the above utxo.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{utxo}, nil).Once()

	// Check that the error is returned from createWalletTxInput.
	err = set.AddWalletInputs(wallet)
	require.Error(t, err)

	// Mock the wallet to return empty utxos.
	wallet.On("ListUnspentWitnessFromDefaultAccount",
		min, max).Return([]*lnwallet.Utxo{}, nil).Once()

	// Check that the error is returned from not having wallet inputs.
	err = set.AddWalletInputs(wallet)
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
	err := set.AddWalletInputs(wallet)
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
	err := set.AddWalletInputs(wallet)
	require.NoError(t, err)
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

	// Add wallet inputs to the input set, which should give us an error as
	// the wallet cannot cover the budget.
	err = set.AddWalletInputs(wallet)
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
