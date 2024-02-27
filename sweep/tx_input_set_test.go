package sweep

import (
	"errors"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestTxInputSet tests adding various sized inputs to the set.
func TestTxInputSet(t *testing.T) {
	const (
		feeRate   = 1000
		maxInputs = 10
	)
	set := newTxInputSet(feeRate, 0, maxInputs)

	// Create a 300 sat input. The fee to sweep this input to a P2WKH output
	// is 439 sats. That means that this input yields -139 sats and we
	// expect it not to be added.
	if set.add(createP2WKHInput(300), constraintsRegular) {
		t.Fatal("expected add of negatively yielding input to fail")
	}

	// A 700 sat input should be accepted into the set, because it yields
	// positively.
	if !set.add(createP2WKHInput(700), constraintsRegular) {
		t.Fatal("expected add of positively yielding input to succeed")
	}

	fee := set.weightEstimate(true).fee()
	require.Equal(t, btcutil.Amount(487), fee)

	// The tx output should now be 700-487 = 213 sats. The dust limit isn't
	// reached yet.
	if set.totalOutput() != 213 {
		t.Fatal("unexpected output value")
	}
	if set.enoughInput() {
		t.Fatal("expected dust limit not yet to be reached")
	}

	// Add a 1000 sat input. This increases the tx fee to 760 sats. The tx
	// output should now be 1000+700 - 760 = 940 sats.
	if !set.add(createP2WKHInput(1000), constraintsRegular) {
		t.Fatal("expected add of positively yielding input to succeed")
	}
	if set.totalOutput() != 940 {
		t.Fatal("unexpected output value")
	}
	if !set.enoughInput() {
		t.Fatal("expected dust limit to be reached")
	}
}

// TestTxInputSetFromWallet tests adding a wallet input to a TxInputSet to reach
// the dust limit.
func TestTxInputSetFromWallet(t *testing.T) {
	const (
		feeRate   = 500
		maxInputs = 10
	)

	wallet := &mockWallet{}
	set := newTxInputSet(feeRate, 0, maxInputs)

	// Add a 500 sat input to the set. It yields positively, but doesn't
	// reach the output dust limit.
	if !set.add(createP2WKHInput(500), constraintsRegular) {
		t.Fatal("expected add of positively yielding input to succeed")
	}
	if set.enoughInput() {
		t.Fatal("expected dust limit not yet to be reached")
	}

	// Expect that adding a negative yield input fails.
	if set.add(createP2WKHInput(50), constraintsRegular) {
		t.Fatal("expected negative yield input add to fail")
	}

	// Force add the negative yield input. It should succeed.
	if !set.add(createP2WKHInput(50), constraintsForce) {
		t.Fatal("expected forced add to succeed")
	}

	err := set.AddWalletInputs(wallet)
	if err != nil {
		t.Fatal(err)
	}

	if !set.enoughInput() {
		t.Fatal("expected dust limit to be reached")
	}
}

// createP2WKHInput returns a P2WKH test input with the specified amount.
func createP2WKHInput(amt btcutil.Amount) input.Input {
	input := createTestInput(int64(amt), input.WitnessKeyHash)
	return &input
}

type mockWallet struct {
	Wallet
}

func (m *mockWallet) ListUnspentWitnessFromDefaultAccount(minConfs, maxConfs int32) (
	[]*lnwallet.Utxo, error) {

	return []*lnwallet.Utxo{
		{
			AddressType: lnwallet.WitnessPubKey,
			Value:       10000,
		},
	}, nil
}

type reqInput struct {
	input.Input

	txOut *wire.TxOut
}

func (r *reqInput) RequiredTxOut() *wire.TxOut {
	return r.txOut
}

// TestTxInputSetRequiredOutput tests that the tx input set behaves as expected
// when we add inputs that have required tx outs.
func TestTxInputSetRequiredOutput(t *testing.T) {
	const (
		feeRate   = 1000
		maxInputs = 10
	)
	set := newTxInputSet(feeRate, 0, maxInputs)

	// Attempt to add an input with a required txout below the dust limit.
	// This should fail since we cannot trim such outputs.
	inp := &reqInput{
		Input: createP2WKHInput(500),
		txOut: &wire.TxOut{
			Value:    500,
			PkScript: make([]byte, input.P2PKHSize),
		},
	}
	require.False(t, set.add(inp, constraintsRegular),
		"expected adding dust required tx out to fail")

	// Create a 1000 sat input that also has a required TxOut of 1000 sat.
	// The fee to sweep this input to a P2WKH output is 439 sats.
	inp = &reqInput{
		Input: createP2WKHInput(1000),
		txOut: &wire.TxOut{
			Value:    1000,
			PkScript: make([]byte, input.P2WPKHSize),
		},
	}
	require.True(t, set.add(inp, constraintsRegular), "failed adding input")

	// The fee needed to pay for this input and output should be 439 sats.
	fee := set.weightEstimate(false).fee()
	require.Equal(t, btcutil.Amount(439), fee)

	// Since the tx set currently pays no fees, we expect the current
	// change to actually be negative, since this is what it would cost us
	// in fees to add a change output.
	feeWithChange := set.weightEstimate(true).fee()
	if set.changeOutput != -feeWithChange {
		t.Fatalf("expected negative change of %v, had %v",
			-feeWithChange, set.changeOutput)
	}

	// This should also be reflected by not having enough input.
	require.False(t, set.enoughInput())

	// Get a weight estimate without change output, and add an additional
	// input to it.
	dummyInput := createP2WKHInput(1000)
	weight := set.weightEstimate(false)
	require.NoError(t, weight.add(dummyInput))

	// Now we add a an input that is large enough to pay the fee for the
	// transaction without a change output, but not large enough to afford
	// adding a change output.
	extraInput1 := weight.fee() + 100
	require.True(t, set.add(createP2WKHInput(extraInput1), constraintsRegular),
		"expected add of positively yielding input to succeed")

	// The change should be negative, since we would have to add a change
	// output, which we cannot yet afford.
	if set.changeOutput >= 0 {
		t.Fatal("expected change to be negaitve")
	}

	// Even though we cannot afford a change output, the tx set is valid,
	// since we can pay the fees without the change output.
	require.True(t, set.enoughInput())

	// Get another weight estimate, this time with a change output, and
	// figure out how much we must add to afford a change output.
	weight = set.weightEstimate(true)
	require.NoError(t, weight.add(dummyInput))

	// We add what is left to reach this value.
	extraInput2 := weight.fee() - extraInput1 + 100

	// Add this input, which should result in the change now being 100 sats.
	require.True(t, set.add(createP2WKHInput(extraInput2), constraintsRegular))

	// The change should be 100, since this is what is left after paying
	// fees in case of a change output.
	change := set.changeOutput
	if change != 100 {
		t.Fatalf("expected change be 100, was %v", change)
	}

	// Even though the change output is dust, we have enough for fees, and
	// we have an output, so it should be considered enough to craft a
	// valid sweep transaction.
	require.True(t, set.enoughInput())

	// Finally we add an input that should push the change output above the
	// dust limit.
	weight = set.weightEstimate(true)
	require.NoError(t, weight.add(dummyInput))

	// We expect the change to everything that is left after paying the tx
	// fee.
	extraInput3 := weight.fee() - extraInput1 - extraInput2 + 1000
	require.True(t, set.add(createP2WKHInput(extraInput3), constraintsRegular))

	change = set.changeOutput
	if change != 1000 {
		t.Fatalf("expected change to be %v, had %v", 1000, change)
	}
	require.True(t, set.enoughInput())
}

// TestNewBudgetInputSet checks `NewBudgetInputSet` correctly validates the
// supplied inputs and returns the error.
func TestNewBudgetInputSet(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Pass an empty slice and expect an error.
	set, err := NewBudgetInputSet([]pendingInput{})
	rt.ErrorContains(err, "inputs slice is empty")
	rt.Nil(set)

	// Create two inputs with different deadline heights.
	inp0 := createP2WKHInput(1000)
	inp1 := createP2WKHInput(1000)
	inp2 := createP2WKHInput(1000)
	input0 := pendingInput{
		Input: inp0,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.None[int32](),
		},
	}
	input1 := pendingInput{
		Input: inp1,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.Some(int32(1)),
		},
	}
	input2 := pendingInput{
		Input: inp2,
		params: Params{
			Budget:         100,
			DeadlineHeight: fn.Some(int32(2)),
		},
	}

	// Pass a slice of inputs with different deadline heights.
	set, err = NewBudgetInputSet([]pendingInput{input1, input2})
	rt.ErrorContains(err, "inputs have different deadline heights")
	rt.Nil(set)

	// Pass a slice of inputs that only one input has the deadline height.
	set, err = NewBudgetInputSet([]pendingInput{input0, input2})
	rt.NoError(err)
	rt.NotNil(set)

	// Pass a slice of inputs that are duplicates.
	set, err = NewBudgetInputSet([]pendingInput{input1, input1})
	rt.ErrorContains(err, "duplicate inputs")
	rt.Nil(set)
}

// TestBudgetInputSetAddInput checks that `addInput` correctly updates the
// budget of the input set.
func TestBudgetInputSetAddInput(t *testing.T) {
	t.Parallel()

	// Create a testing input with a budget of 100 satoshis.
	input := createP2WKHInput(1000)
	pi := &pendingInput{
		Input: input,
		params: Params{
			Budget: 100,
		},
	}

	// Initialize an input set, which adds the above input.
	set, err := NewBudgetInputSet([]pendingInput{*pi})
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

// TestNeedWalletInput checks that NeedWalletInput correctly determines if a
// wallet input is needed.
func TestNeedWalletInput(t *testing.T) {
	t.Parallel()

	// Create a mock input that doesn't have required outputs.
	mockInput := &input.MockInput{}
	mockInput.On("RequiredTxOut").Return(nil)
	defer mockInput.AssertExpectations(t)

	// Create a mock input that has required outputs.
	mockInputRequireOutput := &input.MockInput{}
	mockInputRequireOutput.On("RequiredTxOut").Return(&wire.TxOut{})
	defer mockInputRequireOutput.AssertExpectations(t)

	// We now create two pending inputs each has a budget of 100 satoshis.
	const budget = 100

	// Create the pending input that doesn't have a required output.
	piBudget := &pendingInput{
		Input:  mockInput,
		params: Params{Budget: budget},
	}

	// Create the pending input that has a required output.
	piRequireOutput := &pendingInput{
		Input:  mockInputRequireOutput,
		params: Params{Budget: budget},
	}

	testCases := []struct {
		name        string
		setupInputs func() []*pendingInput
		need        bool
	}{
		{
			// When there are no pending inputs, we won't need a
			// wallet input. Technically this should be an invalid
			// state.
			name: "no inputs",
			setupInputs: func() []*pendingInput {
				return nil
			},
			need: false,
		},
		{
			// When there's no required output, we don't need a
			// wallet input.
			name: "no required outputs",
			setupInputs: func() []*pendingInput {
				// Create a sign descriptor to be used in the
				// pending input when calculating budgets can
				// be borrowed.
				sd := &input.SignDescriptor{
					Output: &wire.TxOut{
						Value: budget,
					},
				}
				mockInput.On("SignDesc").Return(sd).Once()

				return []*pendingInput{piBudget}
			},
			need: false,
		},
		{
			// When the output value cannot cover the budget, we
			// need a wallet input.
			name: "output value cannot cover budget",
			setupInputs: func() []*pendingInput {
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
					&wire.OutPoint{Hash: chainhash.Hash{1}},
				).Maybe()
				mockInput.On("WitnessType").Return(
					input.CommitmentAnchor,
				).Maybe()

				return []*pendingInput{piBudget}
			},
			need: true,
		},
		{
			// When there's only inputs that require outputs, we
			// need wallet inputs.
			name: "only required outputs",
			setupInputs: func() []*pendingInput {
				return []*pendingInput{piRequireOutput}
			},
			need: true,
		},
		{
			// When there's a mix of inputs, but the borrowable
			// budget cannot cover the required, we need a wallet
			// input.
			name: "not enough budget to be borrowed",
			setupInputs: func() []*pendingInput {
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

				return []*pendingInput{
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
			setupInputs: func() []*pendingInput {
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

				return []*pendingInput{
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

			// Initialize an input set, which adds the testing
			// inputs.
			set := &BudgetInputSet{inputs: inputs}

			result := set.NeedWalletInput()
			require.Equal(t, tc.need, result)
		})
	}
}

// TestAddWalletInputReturnErr tests the three possible errors returned from
// AddWalletInputs:
// - error from ListUnspentWitnessFromDefaultAccount.
// - error from createWalletTxInput.
// - error when wallet doesn't have utxos.
func TestAddWalletInputReturnErr(t *testing.T) {
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

// TestAddWalletInputNotEnoughInputs checks that when there are not enough
// wallet utxos, an error is returned and the budget set is reset to its
// initial state.
func TestAddWalletInputNotEnoughInputs(t *testing.T) {
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
	pi := &pendingInput{
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
	set := BudgetInputSet{inputs: []*pendingInput{pi}}

	// Add wallet inputs to the input set, which should give us an error as
	// the wallet cannot cover the budget.
	err := set.AddWalletInputs(wallet)
	require.ErrorIs(t, err, ErrNotEnoughInputs)

	// Check that the budget set is reverted to its initial state.
	require.Len(t, set.inputs, 1)
	require.Equal(t, pi, set.inputs[0])
}

// TestAddWalletInputSuccess checks that when there are enough wallet utxos,
// they are added to the input set.
func TestAddWalletInputSuccess(t *testing.T) {
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
	pi := &pendingInput{
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
	mockInput.On("OutPoint").Return(&wire.OutPoint{Hash: chainhash.Hash{1}})
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
	set, err := NewBudgetInputSet([]pendingInput{*pi})
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
	require.Equal(t, deadline, set.DeadlineHeight().UnsafeFromSome())
	// Weak check, a strong check is to open the slice and check each item.
	require.Len(t, set.inputs, 3)
}
