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

// txInputSet is an object that accumulates tx inputs and keeps running counters
// on various properties of the tx.
type txInputSet struct {
	// weightEstimate is the (worst case) tx weight with the current set of
	// inputs.
	weightEstimate input.TxWeightEstimator

	// inputTotal is the total value of all inputs.
	inputTotal btcutil.Amount

	// outputValue is the value of the tx output.
	outputValue btcutil.Amount

	// feePerKW is the fee rate used to calculate the tx fee.
	feePerKW chainfee.SatPerKWeight

	// inputs is the set of tx inputs.
	inputs []input.Input

	// dustLimit is the minimum output value of the tx.
	dustLimit btcutil.Amount

	// maxInputs is the maximum number of inputs that will be accepted in
	// the set.
	maxInputs int

	// walletInputTotal is the total value of inputs coming from the wallet.
	walletInputTotal btcutil.Amount

	// wallet contains wallet functionality required by the input set to
	// retrieve utxos.
	wallet Wallet

	// force indicates that this set must be swept even if the total yield
	// is negative.
	force bool
}

// newTxInputSet constructs a new, empty input set.
func newTxInputSet(wallet Wallet, feePerKW,
	relayFee chainfee.SatPerKWeight, maxInputs int) *txInputSet {

	dustLimit := txrules.GetDustThreshold(
		input.P2WPKHSize,
		btcutil.Amount(relayFee.FeePerKVByte()),
	)

	b := txInputSet{
		feePerKW:  feePerKW,
		dustLimit: dustLimit,
		maxInputs: maxInputs,
		wallet:    wallet,
	}

	// Add the sweep tx output to the weight estimate.
	b.weightEstimate.AddP2WKHOutput()

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
func (t *txInputSet) add(input input.Input, constraints addConstraints) bool {
	// Stop if max inputs is reached. Do not count additional wallet inputs,
	// because we don't know in advance how many we may need.
	if constraints != constraintsWallet &&
		len(t.inputs) >= t.maxInputs {

		return false
	}

	// Can ignore error, because it has already been checked when
	// calculating the yields.
	size, isNestedP2SH, _ := input.WitnessType().SizeUpperBound()

	// Add weight of this new candidate input to a copy of the weight
	// estimator.
	newWeightEstimate := t.weightEstimate
	if isNestedP2SH {
		newWeightEstimate.AddNestedP2WSHInput(size)
	} else {
		newWeightEstimate.AddWitnessInput(size)
	}

	value := btcutil.Amount(input.SignDesc().Output.Value)
	newInputTotal := t.inputTotal + value

	weight := newWeightEstimate.Weight()
	fee := t.feePerKW.FeeForWeight(int64(weight))

	// Calculate the output value if the current input would be
	// added to the set.
	newOutputValue := newInputTotal - fee

	// Initialize new wallet total with the current wallet total. This is
	// updated below if this input is a wallet input.
	newWalletTotal := t.walletInputTotal

	// Calculate the yield of this input from the change in tx output value.
	inputYield := newOutputValue - t.outputValue

	switch constraints {

	// Don't sweep inputs that cost us more to sweep than they give us.
	case constraintsRegular:
		if inputYield <= 0 {
			return false
		}

	// For force adds, no further constraints apply.
	case constraintsForce:
		t.force = true

	// We are attaching a wallet input to raise the tx output value above
	// the dust limit.
	case constraintsWallet:
		// Skip this wallet input if adding it would lower the output
		// value.
		if inputYield <= 0 {
			return false
		}

		// Calculate the total value that we spend in this tx from the
		// wallet if we'd add this wallet input.
		newWalletTotal += value

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
		if !t.force && newWalletTotal >= newOutputValue {
			log.Debugf("Rejecting wallet input of %v, because it "+
				"would make a negative yielding transaction "+
				"(%v)",
				value, newOutputValue-newWalletTotal)

			return false
		}
	}

	// Update running values.
	//
	// TODO: Return new instance?
	t.inputTotal = newInputTotal
	t.outputValue = newOutputValue
	t.inputs = append(t.inputs, input)
	t.weightEstimate = newWeightEstimate
	t.walletInputTotal = newWalletTotal

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
