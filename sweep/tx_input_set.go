package sweep

import (
	"fmt"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
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

	// AddWalletInputs adds wallet inputs to the set until its budget is
	// covered, subject to the wallet input count and transaction weight
	// limits. Inputs that cannot pay for their own weight at the relay fee
	// rate are skipped. Return an error if no input can contribute to fees.
	AddWalletInputs(wallet Wallet,
		relayFeeRate chainfee.SatPerKWeight) error

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

	// Immediate returns a boolean to indicate whether the tx made from
	// this input set should be published immediately.
	//
	// TODO(yy): create a new method `Params` to combine the informational
	// methods DeadlineHeight, Budget, StartingFeeRate and Immediate.
	Immediate() bool
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

	// extraBudget is a value that should be allocated to sweep the given
	// set of inputs. This can be used to add extra funds to the sweep
	// transaction, for example to cover fees for additional outputs of
	// custom channels.
	extraBudget btcutil.Amount
}

// Compile-time constraint to ensure budgetInputSet implements InputSet.
var _ InputSet = (*BudgetInputSet)(nil)

// errEmptyInputs is returned when the input slice is empty.
var errEmptyInputs = fmt.Errorf("inputs slice is empty")

const (
	// maxStandardTxWeight is the maximum weight a transaction may have to
	// be accepted into the mempool under the default standardness policy.
	// It mirrors btcd's unexported maxStandardTxWeight.
	maxStandardTxWeight = lntypes.WeightUnit(400_000)

	// sweepTxWeightMargin is the weight reserved for transaction outputs
	// that are not yet known when wallet inputs are selected, such as
	// extra outputs added by the aux sweeper.
	sweepTxWeightMargin = lntypes.WeightUnit(1_000)
)

// validateInputs is used when creating new BudgetInputSet to ensure there are
// no duplicate inputs and they all share the same deadline heights, if set.
func validateInputs(inputs []SweeperInput, deadlineHeight int32) error {
	// Sanity check the input slice to ensure it's non-empty.
	if len(inputs) == 0 {
		return errEmptyInputs
	}

	// inputDeadline tracks the input's deadline height. It will be updated
	// if the input has a different deadline than the specified
	// deadlineHeight.
	inputDeadline := deadlineHeight

	// dedupInputs is a set used to track unique outpoints of the inputs.
	dedupInputs := fn.NewSet(
		// Iterate all the inputs and map the function.
		fn.Map(inputs, func(inp SweeperInput) wire.OutPoint {
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
		})...,
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
func NewBudgetInputSet(inputs []SweeperInput, deadlineHeight int32,
	auxSweeper fn.Option[AuxSweeper]) (*BudgetInputSet, error) {

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

	// Attach an optional budget. This will be a no-op if the auxSweeper
	// is not set.
	if err := bi.attachExtraBudget(auxSweeper); err != nil {
		return nil, err
	}

	return bi, nil
}

// attachExtraBudget attaches an extra budget to the input set, if the passed
// aux sweeper is set.
func (b *BudgetInputSet) attachExtraBudget(s fn.Option[AuxSweeper]) error {
	extraBudget, err := fn.MapOptionZ(
		s, func(aux AuxSweeper) fn.Result[btcutil.Amount] {
			return aux.ExtraBudgetForInputs(b.Inputs())
		},
	).Unpack()
	if err != nil {
		return err
	}

	b.extraBudget = extraBudget

	return nil
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

// addWalletInput takes a wallet UTXO and adds it as an input to be used as
// budget for the input set.
func (b *BudgetInputSet) addWalletInput(utxo *lnwallet.Utxo) error {
	walletInput, err := createWalletTxInput(utxo)
	if err != nil {
		return err
	}

	b.addWalletTxInput(walletInput, utxo.Value)

	return nil
}

// addWalletTxInput adds an already constructed wallet input to the set.
func (b *BudgetInputSet) addWalletTxInput(walletInput input.Input,
	value btcutil.Amount) {

	pi := SweeperInput{
		Input: walletInput,
		params: Params{
			DeadlineHeight: fn.Some(b.deadlineHeight),
		},
	}
	b.addInput(pi)

	log.Debugf("Added wallet input to input set: op=%v, amt=%v",
		pi.OutPoint(), value)
}

// NeedWalletInput returns true if the input set needs more wallet inputs.
//
// A set may need wallet inputs when it has a required output or its total
// value cannot cover its total budget.
func (b *BudgetInputSet) NeedWalletInput() bool {
	var (
		// budgetNeeded is the amount that needs to be covered from
		// other inputs. We start at the value of the extra budget,
		// which might be needed for custom channels that add extra
		// outputs.
		budgetNeeded = b.extraBudget

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
		// This amount can then be lent to the above input. For a wallet
		// input, its `Budget` is set to zero, which means the whole
		// input can be borrowed to cover the budget.
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

// hasNormalInput return a bool to indicate whether there exists an input that
// doesn't require a TxOut. When an input has no required outputs, it's either a
// wallet input, or an input we want to sweep.
func (b *BudgetInputSet) hasNormalInput() bool {
	for _, inp := range b.inputs {
		if inp.RequiredTxOut() != nil {
			continue
		}

		return true
	}

	return false
}

// walletCandidate is a wallet UTXO considered for a budget input set, along
// with the weight it adds to the sweep transaction.
type walletCandidate struct {
	utxo        *lnwallet.Utxo
	walletInput input.Input
	weight      lntypes.WeightUnit
}

// denserThan returns true if the candidate has a strictly higher value per
// weight unit than other. The ratios are compared exactly by cross
// multiplication, as a rounded fee rate can order two candidates on opposite
// sides of the relay fee threshold incorrectly.
func (c walletCandidate) denserThan(other walletCandidate) bool {
	return int64(c.utxo.Value)*int64(other.weight) >
		int64(other.utxo.Value)*int64(c.weight)
}

// AddWalletInputs adds wallet inputs to the set until the specified budget is
// met. When sweeping inputs with required outputs, although there's budget
// specified, it cannot be directly spent from these required outputs. Instead,
// we need to borrow budget from other inputs to make the sweep happen.
// There are two sources to borrow from: 1) other inputs, 2) wallet utxos. If
// we are calling this method, it means other inputs cannot cover the specified
// budget, so we need to borrow from wallet utxos.
//
// Wallet UTXOs are considered in order of value per weight unit, highest
// first. For UTXOs of the same address type this is largest first, so the
// budget is covered with the fewest inputs. Selection stops before the sweep
// would exceed the standard transaction weight limit.
// A UTXO that cannot pay for its own added weight at the node's relay fee
// rate is not useful to any relayable sweep and is skipped.
//
// Return an error if none of the set's inputs can contribute to fees.
//
// NOTE: must be called with the wallet lock held via `WithCoinSelectLock`.
func (b *BudgetInputSet) AddWalletInputs(wallet Wallet,
	relayFeeRate chainfee.SatPerKWeight) error {
	// Retrieve wallet utxos. Only consider confirmed utxos to prevent
	// problems around RBF rules for unconfirmed inputs. This currently
	// ignores the configured coin selection strategy.
	utxos, err := wallet.ListUnspentWitnessFromDefaultAccount(
		1, math.MaxInt32,
	)
	if err != nil {
		return fmt.Errorf("list unspent witness: %w", err)
	}

	// Seed a weight estimate with the set's existing inputs and reserve an
	// output for the sweep destination. This keeps the transaction below
	// the standardness weight limit as wallet inputs are added.
	var estimator input.TxWeightEstimator
	for _, inp := range b.inputs {
		err := inp.WitnessType().AddWeightEstimation(&estimator)
		if err != nil {
			// Skip inputs that cannot be weighted, the same way the
			// tx generator treats them. They are accounted for by
			// the weight margin.
			log.Errorf("Failed to get weight estimate for "+
				"input=%v: %v", inp.OutPoint(), err)

			continue
		}

		if txOut := inp.RequiredTxOut(); txOut != nil {
			estimator.AddTxOutput(txOut)
		}
	}
	estimator.AddP2TROutput()

	// Measure the weight each UTXO adds to the transaction, without
	// mutating the estimator.
	candidates := make([]walletCandidate, 0, len(utxos))
	for _, utxo := range utxos {
		inp, err := createWalletTxInput(utxo)
		if err != nil {
			return err
		}

		trial := estimator
		err = inp.WitnessType().AddWeightEstimation(&trial)
		if err != nil {
			return fmt.Errorf("estimate wallet input weight: %w",
				err)
		}

		candidates = append(candidates, walletCandidate{
			utxo:        utxo,
			walletInput: inp,
			weight:      trial.Weight() - estimator.Weight(),
		})
	}

	// Consider the UTXOs with the highest value per weight unit first. For
	// UTXOs of the same address type this is largest first, so the fewest
	// inputs cover the budget.
	//
	// TODO(yy): add more choices to CoinSelectionStrategy and use the
	// configured value here.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].denserThan(candidates[j])
	})

	maxWeight := maxStandardTxWeight - sweepTxWeightMargin

	for _, c := range candidates {
		// A wallet input must contribute more value than the fee for
		// its own weight at the minimum relay fee rate. Because
		// candidates are sorted by exact value density, all remaining
		// candidates are also unable to pay for themselves once this
		// condition is met. Since the value is a whole number of
		// satoshis, comparing it against the rounded down fee is the
		// same as comparing the exact value density against the rate.
		if c.utxo.Value <= relayFeeRate.FeeForWeight(c.weight) {
			log.Debugf("Stopped adding wallet inputs: input %v "+
				"with value %v cannot pay for weight %v at "+
				"relay fee rate %v", c.utxo.OutPoint,
				c.utxo.Value, c.weight, relayFeeRate)

			break
		}

		weight := estimator.Weight()

		// Stop adding wallet inputs once the transaction would exceed
		// the standardness weight limit.
		if weight+c.weight > maxWeight {
			log.Warnf("Stopped adding wallet inputs: tx weight %v "+
				"would exceed %v", weight+c.weight, maxWeight)

			break
		}

		err := c.walletInput.WitnessType().AddWeightEstimation(
			&estimator,
		)
		if err != nil {
			return fmt.Errorf("estimate wallet input weight: %w",
				err)
		}

		b.addWalletTxInput(c.walletInput, c.utxo.Value)

		// Return if we've reached the minimum output amount.
		if !b.NeedWalletInput() {
			return nil
		}
	}

	// Exit if there are no inputs can contribute to the fees.
	if !b.hasNormalInput() {
		return ErrNotEnoughInputs
	}

	// If there's at least one input that can contribute to fees, we allow
	// the sweep to continue, even though the full budget can't be met.
	// Maybe later more wallet inputs will become available and we can add
	// them if needed.
	budget := b.Budget()
	total, spendable := b.inputAmts()
	log.Warnf("Not enough wallet UTXOs: need budget=%v, has spendable=%v, "+
		"total=%v, missing at least %v, sweeping anyway...", budget,
		spendable, total, budget-spendable)

	return nil
}

// Budget returns the total budget of the set.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) Budget() btcutil.Amount {
	budget := btcutil.Amount(0)
	for _, input := range b.inputs {
		budget += input.params.Budget
	}

	// We'll also tack on the extra budget which will eventually be
	// accounted for by the wallet txns when we're broadcasting.
	return budget + b.extraBudget
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

// inputAmts returns two values for the set - the total input amount, and the
// spendable amount. Only the spendable amount can be used to pay the fees.
func (b *BudgetInputSet) inputAmts() (btcutil.Amount, btcutil.Amount) {
	var (
		totalAmt     btcutil.Amount
		spendableAmt btcutil.Amount
	)

	for _, inp := range b.inputs {
		output := btcutil.Amount(inp.SignDesc().Output.Value)
		totalAmt += output

		if inp.RequiredTxOut() != nil {
			continue
		}

		spendableAmt += output
	}

	return totalAmt, spendableAmt
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

// Immediate returns whether the inputs should be swept immediately.
//
// NOTE: part of the InputSet interface.
func (b *BudgetInputSet) Immediate() bool {
	for _, inp := range b.inputs {
		// As long as one of the inputs is immediate, the whole set is
		// immediate.
		if inp.params.Immediate {
			return true
		}
	}

	return false
}
