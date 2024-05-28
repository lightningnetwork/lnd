package sweep

import (
	"fmt"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrNotEnoughInputs is returned when there are not enough wallet
	// inputs to construct a non-dust change output for an input set.
	ErrNotEnoughInputs = fmt.Errorf("not enough inputs")

	// ErrDeadlinesMismatch is returned when the deadlines of the input
	// sets do not match.
	ErrDeadlinesMismatch = fmt.Errorf("deadlines mismatch")

	// ErrDustOutput is returned when the output value is below the dust
	// limit.
	ErrDustOutput = fmt.Errorf("dust output")
)

// InputSet defines an interface that's responsible for filtering a set of
// inputs that can be swept economically.
type InputSet interface {
	// Inputs returns the set of inputs that should be used to create a tx.
	Inputs() []input.Input

	// AddWalletInputs adds wallet inputs to the set until a non-dust
	// change output can be made. Return an error if there are not enough
	// wallet inputs.
	AddWalletInputs(wallet Wallet) error

	// NeedWalletInput returns true if the input set needs more wallet
	// inputs.
	NeedWalletInput() bool

	// DeadlineHeight returns an absolute block height to express the
	// time-sensitivity of the input set. The outputs from a force close tx
	// have different time preferences:
	// - to_local: no time pressure as it can only be swept by us.
	// - first level outgoing HTLC: must be swept before its corresponding
	//   incoming HTLC's CLTV is reached.
	// - first level incoming HTLC: must be swept before its CLTV is
	//   reached.
	// - second level HTLCs: no time pressure.
	// - anchor: for CPFP-purpose anchor, it must be swept before any of
	//   the above CLTVs is reached. For non-CPFP purpose anchor, there's
	//   no time pressure.
	DeadlineHeight() int32

	// Budget givens the total amount that can be used as fees by this
	// input set.
	Budget() btcutil.Amount

	// StartingFeeRate returns the max starting fee rate found in the
	// inputs.
	StartingFeeRate() fn.Option[chainfee.SatPerKWeight]
}

// createWalletTxInput converts a wallet utxo into an object that can be added
// to the other inputs to sweep.
func createWalletTxInput(utxo *lnwallet.Utxo) (input.Input, error) {
	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			PkScript: utxo.PkScript,
			Value:    int64(utxo.Value),
		},
		HashType: txscript.SigHashAll,
	}

	var witnessType input.WitnessType
	switch utxo.AddressType {
	case lnwallet.WitnessPubKey:
		witnessType = input.WitnessKeyHash
	case lnwallet.NestedWitnessPubKey:
		witnessType = input.NestedWitnessKeyHash
	case lnwallet.TaprootPubkey:
		witnessType = input.TaprootPubKeySpend
		signDesc.HashType = txscript.SigHashDefault
	default:
		return nil, fmt.Errorf("unknown address type %v",
			utxo.AddressType)
	}

	// A height hint doesn't need to be set, because we don't monitor these
	// inputs for spend.
	heightHint := uint32(0)

	return input.NewBaseInput(
		&utxo.OutPoint, witnessType, signDesc, heightHint,
	), nil
}

// BudgetInputSet implements the interface `InputSet`. It takes a list of
// pending inputs which share the same deadline height and groups them into a
// set conditionally based on their economical values.
type BudgetInputSet struct {
	// inputs is the set of inputs that have been added to the set after
	// considering their economical contribution.
	inputs []*SweeperInput

	// deadlineHeight is the height which the inputs in this set must be
	// confirmed by.
	deadlineHeight int32
}

// Compile-time constraint to ensure budgetInputSet implements InputSet.
var _ InputSet = (*BudgetInputSet)(nil)

// validateInputs is used when creating new BudgetInputSet to ensure there are
// no duplicate inputs and they all share the same deadline heights, if set.
func validateInputs(inputs []SweeperInput, deadlineHeight int32) error {
	// Sanity check the input slice to ensure it's non-empty.
	if len(inputs) == 0 {
		return fmt.Errorf("inputs slice is empty")
	}

	// inputDeadline tracks the input's deadline height. It will be updated
	// if the input has a different deadline than the specified
	// deadlineHeight.
	inputDeadline := deadlineHeight

	// dedupInputs is a set used to track unique outpoints of the inputs.
	dedupInputs := fn.NewSet(
		// Iterate all the inputs and map the function.
		fn.Map(func(inp SweeperInput) wire.OutPoint {
			// If the input has a deadline height, we'll check if
			// it's the same as the specified.
			inp.params.DeadlineHeight.WhenSome(func(h int32) {
				// Exit early if the deadlines matched.
				if h == deadlineHeight {
					return
				}

				// Update the deadline height if it's
				// different.
				inputDeadline = h
			})

			return inp.OutPoint()
		}, inputs)...,
	)

	// Make sure the inputs share the same deadline height when there is
	// one.
	if inputDeadline != deadlineHeight {
		return fmt.Errorf("input deadline height not matched: want "+
			"%d, got %d", deadlineHeight, inputDeadline)
	}

	// Provide a defensive check to ensure that we don't have any duplicate
	// inputs within the set.
	if len(dedupInputs) != len(inputs) {
		return fmt.Errorf("duplicate inputs")
	}

	return nil
}

// NewBudgetInputSet creates a new BudgetInputSet.
func NewBudgetInputSet(inputs []SweeperInput,
	deadlineHeight int32) (*BudgetInputSet, error) {

	// Validate the supplied inputs.
	if err := validateInputs(inputs, deadlineHeight); err != nil {
		return nil, err
	}

	bi := &BudgetInputSet{
		deadlineHeight: deadlineHeight,
		inputs:         make([]*SweeperInput, 0, len(inputs)),
	}

	for _, input := range inputs {
		bi.addInput(input)
	}

	log.Tracef("Created %v", bi.String())

	return bi, nil
}

// String returns a human-readable description of the input set.
func (b *BudgetInputSet) String() string {
	inputsDesc := ""
	for _, input := range b.inputs {
		inputsDesc += fmt.Sprintf("\n%v", input)
	}

	return fmt.Sprintf("BudgetInputSet(budget=%v, deadline=%v, "+
		"inputs=[%v])", b.Budget(), b.DeadlineHeight(), inputsDesc)
}

// addInput adds an input to the input set.
func (b *BudgetInputSet) addInput(input SweeperInput) {
	b.inputs = append(b.inputs, &input)
}

// NeedWalletInput returns true if the input set needs more wallet inputs.
//
// A set may need wallet inputs when it has a required output or its total
// value cannot cover its total budget.
func (b *BudgetInputSet) NeedWalletInput() bool {
	var (
		// budgetNeeded is the amount that needs to be covered from
		// other inputs.
		budgetNeeded btcutil.Amount

		// budgetBorrowable is the amount that can be borrowed from
		// other inputs.
		budgetBorrowable btcutil.Amount
	)

	for _, inp := range b.inputs {
		// If this input has a required output, we can assume it's a
		// second-level htlc txns input. Although this input must have
		// a value that can cover its budget, it cannot be used to pay
		// fees. Instead, we need to borrow budget from other inputs to
		// make the sweep happen. Once swept, the input value will be
		// credited to the wallet.
		if inp.RequiredTxOut() != nil {
			budgetNeeded += inp.params.Budget
			continue
		}

		// Get the amount left after covering the input's own budget.
		// This amount can then be lent to the above input.
		budget := inp.params.Budget
		output := btcutil.Amount(inp.SignDesc().Output.Value)
		budgetBorrowable += output - budget

		// If the input's budget is not even covered by itself, we need
		// to borrow outputs from other inputs.
		if budgetBorrowable < 0 {
			log.Tracef("Input %v specified a budget that exceeds "+
				"its output value: %v > %v", inp, budget,
				output)
		}
	}

	log.Debugf("NeedWalletInput: budgetNeeded=%v, budgetBorrowable=%v",
		budgetNeeded, budgetBorrowable)

	// If we don't have enough extra budget to borrow, we need wallet
	// inputs.
	return budgetBorrowable < budgetNeeded
}

// copyInputs returns a copy of the slice of the inputs in the set.
func (b *BudgetInputSet) copyInputs() []*SweeperInput {
	inputs := make([]*SweeperInput, len(b.inputs))
	copy(inputs, b.inputs)
	return inputs
}

// AddWalletInputs adds wallet inputs to the set until the specified budget is
// met. When sweeping inputs with required outputs, although there's budget
// specified, it cannot be directly spent from these required outputs. Instead,
// we need to borrow budget from other inputs to make the sweep happen.
// There are two sources to borrow from: 1) other inputs, 2) wallet utxos. If
// we are calling this method, it means other inputs cannot cover the specified
// budget, so we need to borrow from wallet utxos.
//
// Return an error if there are not enough wallet inputs, and the budget set is
// set to its initial state by removing any wallet inputs added.
//
// NOTE: must be called with the wallet lock held via `WithCoinSelectLock`.
func (b *BudgetInputSet) AddWalletInputs(wallet Wallet) error {
	// Retrieve wallet utxos. Only consider confirmed utxos to prevent
	// problems around RBF rules for unconfirmed inputs. This currently
	// ignores the configured coin selection strategy.
	utxos, err := wallet.ListUnspentWitnessFromDefaultAccount(
		1, math.MaxInt32,
	)
	if err != nil {
		return fmt.Errorf("list unspent witness: %w", err)
	}

	// Sort the UTXOs by putting smaller values at the start of the slice
	// to avoid locking large UTXO for sweeping.
	//
	// TODO(yy): add more choices to CoinSelectionStrategy and use the
	// configured value here.
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].Value < utxos[j].Value
	})

	// Make a copy of the current inputs. If the wallet doesn't have enough
	// utxos to cover the budget, we will revert the current set to its
	// original state by removing the added wallet inputs.
	originalInputs := b.copyInputs()

	// Add wallet inputs to the set until the specified budget is covered.
	for _, utxo := range utxos {
		input, err := createWalletTxInput(utxo)
		if err != nil {
			return err
		}

		pi := SweeperInput{
			Input: input,
			params: Params{
				DeadlineHeight: fn.Some(b.deadlineHeight),
			},
		}
		b.addInput(pi)

		log.Debugf("Added wallet input to input set: op=%v, amt=%v",
			pi.OutPoint(), utxo.Value)

		// Return if we've reached the minimum output amount.
		if !b.NeedWalletInput() {
			return nil
		}
	}

	// The wallet doesn't have enough utxos to cover the budget. Revert the
	// input set to its original state.
	b.inputs = originalInputs

	return ErrNotEnoughInputs
}

// Budget returns the total budget of the set.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) Budget() btcutil.Amount {
	budget := btcutil.Amount(0)
	for _, input := range b.inputs {
		budget += input.params.Budget
	}

	return budget
}

// DeadlineHeight returns the deadline height of the set.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) DeadlineHeight() int32 {
	return b.deadlineHeight
}

// Inputs returns the inputs that should be used to create a tx.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) Inputs() []input.Input {
	inputs := make([]input.Input, 0, len(b.inputs))
	for _, inp := range b.inputs {
		inputs = append(inputs, inp.Input)
	}

	return inputs
}

// StartingFeeRate returns the max starting fee rate found in the inputs.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) StartingFeeRate() fn.Option[chainfee.SatPerKWeight] {
	maxFeeRate := chainfee.SatPerKWeight(0)
	startingFeeRate := fn.None[chainfee.SatPerKWeight]()

	for _, inp := range b.inputs {
		feerate := inp.params.StartingFeeRate.UnwrapOr(0)
		if feerate > maxFeeRate {
			maxFeeRate = feerate
			startingFeeRate = fn.Some(maxFeeRate)
		}
	}

	return startingFeeRate
}
