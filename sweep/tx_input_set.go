package sweep

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// addConstraints defines the constraints to apply when adding an input.
type addConstraints uint8

const (
	// constraintsRegular is for regular input sweeps that should have a positive
	// yield.
	constraintsRegular addConstraints = iota

	// constraintsWallet is for wallet inputs that are only added to bring up the tx
	// output value.
	constraintsWallet

	// constraintsForce is for inputs that should be swept even with a negative
	// yield at the set fee rate.
	constraintsForce
)

type txInputSetState struct {
	// weightEstimate is the (worst case) tx weight with the current set of
	// inputs.
	weightEstimate *weightEstimator

	// inputTotal is the total value of all inputs.
	inputTotal btcutil.Amount

	// outputValue is the value of the tx output.
	outputValue btcutil.Amount

	// inputs is the set of tx inputs.
	inputs []input.Input

	// walletInputTotal is the total value of inputs coming from the wallet.
	walletInputTotal btcutil.Amount

	// force indicates that this set must be swept even if the total yield
	// is negative.
	force bool
}

func (t *txInputSetState) clone() txInputSetState {
	s := txInputSetState{
		weightEstimate:   t.weightEstimate.clone(),
		inputTotal:       t.inputTotal,
		outputValue:      t.outputValue,
		walletInputTotal: t.walletInputTotal,
		force:            t.force,
		inputs:           make([]input.Input, len(t.inputs)),
	}
	copy(s.inputs, t.inputs)

	return s
}

// txInputSet is an object that accumulates tx inputs and keeps running counters
// on various properties of the tx.
type txInputSet struct {
	txInputSetState

	// dustLimit is the minimum output value of the tx.
	dustLimit btcutil.Amount

	// maxInputs is the maximum number of inputs that will be accepted in
	// the set.
	maxInputs int

	// wallet contains wallet functionality required by the input set to
	// retrieve utxos.
	wallet Wallet
}

func dustLimit(relayFee chainfee.SatPerKWeight) btcutil.Amount {
	return txrules.GetDustThreshold(
		input.P2WPKHSize,
		btcutil.Amount(relayFee.FeePerKVByte()),
	)
}

// newTxInputSet constructs a new, empty input set.
func newTxInputSet(wallet Wallet, feePerKW,
	relayFee chainfee.SatPerKWeight, maxInputs int) *txInputSet {

	dustLimit := dustLimit(relayFee)

	state := txInputSetState{
		weightEstimate: newWeightEstimator(feePerKW),
	}

	b := txInputSet{
		dustLimit:       dustLimit,
		maxInputs:       maxInputs,
		wallet:          wallet,
		txInputSetState: state,
	}

	// Add the sweep tx output to the weight estimate.
	b.weightEstimate.addP2WKHOutput()

	return &b
}

// dustLimitReached returns true if we've accumulated enough inputs to meet the
// dust limit.
func (t *txInputSet) dustLimitReached() bool {
	return t.outputValue >= t.dustLimit
}

// add adds a new input to the set. It returns a bool indicating whether the
// input was added to the set. An input is rejected if it decreases the tx
// output value after paying fees.
func (t *txInputSet) addToState(inp input.Input, constraints addConstraints) *txInputSetState {
	// Stop if max inputs is reached. Do not count additional wallet inputs,
	// because we don't know in advance how many we may need.
	if constraints != constraintsWallet &&
		len(t.inputs) >= t.maxInputs {

		return nil
	}

	// Clone the current set state.
	s := t.clone()

	// Add the new input.
	s.inputs = append(s.inputs, inp)

	// Can ignore error, because it has already been checked when
	// calculating the yields.
	_ = s.weightEstimate.add(inp)

	// Add the value of the new input.
	value := btcutil.Amount(inp.SignDesc().Output.Value)
	s.inputTotal += value

	// Recalculate the tx fee.
	fee := s.weightEstimate.fee()

	// Calculate the new output value.
	s.outputValue = s.inputTotal - fee

	// Calculate the yield of this input from the change in tx output value.
	inputYield := s.outputValue - t.outputValue

	switch constraints {

	// Don't sweep inputs that cost us more to sweep than they give us.
	case constraintsRegular:
		if inputYield <= 0 {
			return nil
		}

	// For force adds, no further constraints apply.
	case constraintsForce:
		s.force = true

	// We are attaching a wallet input to raise the tx output value above
	// the dust limit.
	case constraintsWallet:
		// Skip this wallet input if adding it would lower the output
		// value.
		if inputYield <= 0 {
			return nil
		}

		// Calculate the total value that we spend in this tx from the
		// wallet if we'd add this wallet input.
		s.walletInputTotal += value

		// In any case, we don't want to lose money by sweeping. If we
		// don't get more out of the tx then we put in ourselves, do not
		// add this wallet input. If there is at least one force sweep
		// in the set, this does no longer apply.
		//
		// We should only add wallet inputs to get the tx output value
		// above the dust limit, otherwise we'd only burn into fees.
		// This is guarded by tryAddWalletInputsIfNeeded.
		//
		// TODO(joostjager): Possibly require a max ratio between the
		// value of the wallet input and what we get out of this
		// transaction. To prevent attaching and locking a big utxo for
		// very little benefit.
		if !s.force && s.walletInputTotal >= s.outputValue {
			log.Debugf("Rejecting wallet input of %v, because it "+
				"would make a negative yielding transaction "+
				"(%v)",
				value, s.outputValue-s.walletInputTotal)

			return nil
		}
	}

	return &s
}

// add adds a new input to the set. It returns a bool indicating whether the
// input was added to the set. An input is rejected if it decreases the tx
// output value after paying fees.
func (t *txInputSet) add(input input.Input, constraints addConstraints) bool {
	newState := t.addToState(input, constraints)
	if newState == nil {
		return false
	}

	t.txInputSetState = *newState

	return true
}

// addPositiveYieldInputs adds sweepableInputs that have a positive yield to the
// input set. This function assumes that the list of inputs is sorted descending
// by yield.
//
// TODO(roasbeef): Consider including some negative yield inputs too to clean
// up the utxo set even if it costs us some fees up front.  In the spirit of
// minimizing any negative externalities we cause for the Bitcoin system as a
// whole.
func (t *txInputSet) addPositiveYieldInputs(sweepableInputs []txInput) {
	for _, input := range sweepableInputs {
		// Apply relaxed constraints for force sweeps.
		constraints := constraintsRegular
		if input.parameters().Force {
			constraints = constraintsForce
		}

		// Try to add the input to the transaction. If that doesn't
		// succeed because it wouldn't increase the output value,
		// return. Assuming inputs are sorted by yield, any further
		// inputs wouldn't increase the output value either.
		if !t.add(input, constraints) {
			return
		}
	}

	// We managed to add all inputs to the set.
}

// tryAddWalletInputsIfNeeded retrieves utxos from the wallet and tries adding as
// many as required to bring the tx output value above the given minimum.
func (t *txInputSet) tryAddWalletInputsIfNeeded() error {
	// If we've already reached the dust limit, no action is needed.
	if t.dustLimitReached() {
		return nil
	}

	// Retrieve wallet utxos. Only consider confirmed utxos to prevent
	// problems around RBF rules for unconfirmed inputs.
	utxos, err := t.wallet.ListUnspentWitness(1, math.MaxInt32)
	if err != nil {
		return err
	}

	for _, utxo := range utxos {
		input, err := createWalletTxInput(utxo)
		if err != nil {
			return err
		}

		// If the wallet input isn't positively-yielding at this fee
		// rate, skip it.
		if !t.add(input, constraintsWallet) {
			continue
		}

		// Return if we've reached the minimum output amount.
		if t.dustLimitReached() {
			return nil
		}
	}

	// We were not able to reach the minimum output amount.
	return nil
}

// createWalletTxInput converts a wallet utxo into an object that can be added
// to the other inputs to sweep.
func createWalletTxInput(utxo *lnwallet.Utxo) (input.Input, error) {
	var witnessType input.WitnessType
	switch utxo.AddressType {
	case lnwallet.WitnessPubKey:
		witnessType = input.WitnessKeyHash
	case lnwallet.NestedWitnessPubKey:
		witnessType = input.NestedWitnessKeyHash
	default:
		return nil, fmt.Errorf("unknown address type %v",
			utxo.AddressType)
	}

	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			PkScript: utxo.PkScript,
			Value:    int64(utxo.Value),
		},
		HashType: txscript.SigHashAll,
	}

	// A height hint doesn't need to be set, because we don't monitor these
	// inputs for spend.
	heightHint := uint32(0)

	return input.NewBaseInput(
		&utxo.OutPoint, witnessType, signDesc, heightHint,
	), nil
}
