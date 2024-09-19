package chanfunding

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ErrInsufficientFunds is a type matching the error interface which is
// returned when coin selection for a new funding transaction fails due to
// having an insufficient amount of confirmed funds.
type ErrInsufficientFunds struct {
	amountAvailable btcutil.Amount
	amountSelected  btcutil.Amount
}

// Error returns a human-readable string describing the error.
func (e *ErrInsufficientFunds) Error() string {
	return fmt.Sprintf("not enough witness outputs to create funding "+
		"transaction, need %v only have %v available",
		e.amountAvailable, e.amountSelected)
}

// errUnsupportedInput is a type matching the error interface, which is returned
// when trying to calculate the fee of a transaction that references an
// unsupported script in the outpoint of a transaction input.
type errUnsupportedInput struct {
	PkScript []byte
}

// Error returns a human-readable string describing the error.
func (e *errUnsupportedInput) Error() string {
	return fmt.Sprintf("unsupported address type: %x", e.PkScript)
}

// ChangeAddressType is an enum-like type that describes the type of change
// address that should be generated for a transaction.
type ChangeAddressType uint8

const (
	// P2WKHChangeAddress indicates that the change output should be a
	// P2WKH output.
	P2WKHChangeAddress ChangeAddressType = 0

	// P2TRChangeAddress indicates that the change output should be a
	// P2TR output.
	P2TRChangeAddress ChangeAddressType = 1

	// ExistingChangeAddress indicates that the coin selection algorithm
	// should assume an existing output will be used for any change, meaning
	// that the change amount calculated will be added to an existing output
	// and no weight for a new change output should be assumed. The caller
	// must assert that the output value of the selected existing output
	// already is above dust when using this change address type.
	ExistingChangeAddress ChangeAddressType = 2

	// DefaultMaxFeeRatio is the default fee to total amount of outputs
	// ratio that is used to sanity check the fees of a transaction.
	DefaultMaxFeeRatio float64 = 0.2
)

// selectInputs selects a slice of inputs necessary to meet the specified
// selection amount. If input selection is unable to succeed due to insufficient
// funds, a non-nil error is returned. Additionally, the total amount of the
// selected coins are returned in order for the caller to properly handle
// change+fees.
func selectInputs(amt btcutil.Amount, coins []wallet.Coin,
	strategy wallet.CoinSelectionStrategy,
	feeRate chainfee.SatPerKWeight) (btcutil.Amount, []wallet.Coin, error) {

	// All coin selection code in the btcwallet library requires sat/KB.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	arrangedCoins, err := strategy.ArrangeCoins(coins, feeSatPerKB)
	if err != nil {
		return 0, nil, err
	}

	satSelected := btcutil.Amount(0)
	for i, coin := range arrangedCoins {
		satSelected += btcutil.Amount(coin.Value)
		if satSelected >= amt {
			return satSelected, arrangedCoins[:i+1], nil
		}
	}

	return 0, nil, &ErrInsufficientFunds{amt, satSelected}
}

// calculateFees returns for the specified utxos and fee rate two fee
// estimates, one calculated using a change output and one without. The weight
// added to the estimator from a change output is for a P2WKH output.
func calculateFees(utxos []wallet.Coin, feeRate chainfee.SatPerKWeight,
	existingWeight input.TxWeightEstimator,
	changeType ChangeAddressType) (btcutil.Amount, btcutil.Amount, error) {

	weightEstimate := existingWeight
	for _, utxo := range utxos {
		switch {
		case txscript.IsPayToWitnessPubKeyHash(utxo.PkScript):
			weightEstimate.AddP2WKHInput()

		case txscript.IsPayToScriptHash(utxo.PkScript):
			weightEstimate.AddNestedP2WKHInput()

		case txscript.IsPayToTaproot(utxo.PkScript):
			weightEstimate.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)

		default:
			return 0, 0, &errUnsupportedInput{utxo.PkScript}
		}
	}

	// Estimate the fee required for a transaction without a change
	// output.
	totalWeight := weightEstimate.Weight()
	requiredFeeNoChange := feeRate.FeeForWeight(totalWeight)

	// Estimate the fee required for a transaction with a change output.
	switch changeType {
	case P2WKHChangeAddress:
		weightEstimate.AddP2WKHOutput()

	case P2TRChangeAddress:
		weightEstimate.AddP2TROutput()

	case ExistingChangeAddress:
		// Don't add an extra output.

	default:
		return 0, 0, fmt.Errorf("unknown change address type: %v",
			changeType)
	}

	// Now that we have added the change output, redo the fee
	// estimate.
	totalWeight = weightEstimate.Weight()
	requiredFeeWithChange := feeRate.FeeForWeight(totalWeight)

	return requiredFeeNoChange, requiredFeeWithChange, nil
}

// sanityCheckFee checks if the specified fee amounts to what the provided ratio
// allows.
func sanityCheckFee(totalOut, fee btcutil.Amount, maxFeeRatio float64) error {
	// Sanity check the maxFeeRatio itself.
	if maxFeeRatio <= 0.00 || maxFeeRatio > 1.00 {
		return fmt.Errorf("maxFeeRatio must be between 0.00 and 1.00 "+
			"got %.2f", maxFeeRatio)
	}

	maxFee := btcutil.Amount(float64(totalOut) * maxFeeRatio)

	// Check that the fees do not exceed the max allowed value.
	if fee > maxFee {
		return fmt.Errorf("fee %v exceeds max fee (%v) on total "+
			"output value %v with max fee ratio of %.2f", fee,
			maxFee, totalOut, maxFeeRatio)
	}

	// All checks passed, we return nil to signal that the fees are valid.
	return nil
}

// CoinSelect attempts to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhering to the specified fee rate. The
// specified fee rate should be expressed in sat/kw for coin selection to
// function properly.
func CoinSelect(feeRate chainfee.SatPerKWeight, amt, dustLimit btcutil.Amount,
	coins []wallet.Coin, strategy wallet.CoinSelectionStrategy,
	existingWeight input.TxWeightEstimator,
	changeType ChangeAddressType, maxFeeRatio float64) ([]wallet.Coin,
	btcutil.Amount, error) {

	amtNeeded := amt
	for {
		// First perform an initial round of coin selection to estimate
		// the required fee.
		totalSat, selectedUtxos, err := selectInputs(
			amtNeeded, coins, strategy, feeRate,
		)
		if err != nil {
			return nil, 0, err
		}

		// Obtain fee estimates both with and without using a change
		// output.
		requiredFeeNoChange, requiredFeeWithChange, err := calculateFees(
			selectedUtxos, feeRate, existingWeight, changeType,
		)
		if err != nil {
			return nil, 0, err
		}

		changeAmount, newAmtNeeded, err := CalculateChangeAmount(
			totalSat, amt, requiredFeeNoChange,
			requiredFeeWithChange, dustLimit, changeType,
			maxFeeRatio,
		)
		if err != nil {
			return nil, 0, err
		}

		// Need another round, the selected coins aren't enough to pay
		// for the fees.
		if newAmtNeeded != 0 {
			amtNeeded = newAmtNeeded

			continue
		}

		// Coin selection was successful.
		return selectedUtxos, changeAmount, nil
	}
}

// CalculateChangeAmount calculates the change amount being left over when the
// given total amount of sats is provided as inputs for the required output
// amount. The calculation takes into account that we might not want to add a
// change output if the change amount is below the dust limit. The first amount
// returned is the change amount. If that is non-zero, change is left over and
// should be dealt with. The second amount, if non-zero, indicates that the
// total input amount was just not enough to pay for the required amount and
// fees and that more coins need to be selected.
func CalculateChangeAmount(totalInputAmt, requiredAmt, requiredFeeNoChange,
	requiredFeeWithChange, dustLimit btcutil.Amount,
	changeType ChangeAddressType, maxFeeRatio float64) (btcutil.Amount,
	btcutil.Amount, error) {

	// This is just a sanity check to make sure the function is used
	// correctly.
	if changeType == ExistingChangeAddress &&
		requiredFeeNoChange != requiredFeeWithChange {

		return 0, 0, fmt.Errorf("when using existing change address, " +
			"the fees for with or without change must be the same")
	}

	// The difference between the selected amount and the amount
	// requested will be used to pay fees, and generate a change
	// output with the remaining.
	overShootAmt := totalInputAmt - requiredAmt

	var changeAmt btcutil.Amount

	switch {
	// If the excess amount isn't enough to pay for fees based on
	// fee rate and estimated size without using a change output,
	// then increase the requested coin amount by the estimate
	// required fee without using change, performing another round
	// of coin selection.
	case overShootAmt < requiredFeeNoChange:
		return 0, requiredAmt + requiredFeeNoChange, nil

	// If sufficient funds were selected to cover the fee required
	// to include a change output, the remainder will be our change
	// amount.
	case overShootAmt > requiredFeeWithChange:
		changeAmt = overShootAmt - requiredFeeWithChange

	// Otherwise we have selected enough to pay for a tx without a
	// change output.
	default:
		changeAmt = 0
	}

	// In case we would end up with a dust output if we created a
	// change output, we instead just let the dust amount go to
	// fees. Unless we want the change to go to an existing output,
	// in that case we can increase that output value by any amount.
	if changeAmt < dustLimit && changeType != ExistingChangeAddress {
		changeAmt = 0
	}

	// Sanity check the resulting output values to make sure we
	// don't burn a great part to fees.
	totalOut := requiredAmt + changeAmt

	err := sanityCheckFee(totalOut, totalInputAmt-totalOut, maxFeeRatio)
	if err != nil {
		return 0, 0, err
	}

	return changeAmt, 0, nil
}

// CoinSelectSubtractFees attempts to select coins such that we'll spend up to
// amt in total after fees, adhering to the specified fee rate. The selected
// coins, the final output and change values are returned.
func CoinSelectSubtractFees(feeRate chainfee.SatPerKWeight, amt,
	dustLimit btcutil.Amount, coins []wallet.Coin,
	strategy wallet.CoinSelectionStrategy,
	existingWeight input.TxWeightEstimator, changeType ChangeAddressType,
	maxFeeRatio float64) ([]wallet.Coin, btcutil.Amount, btcutil.Amount,
	error) {

	// First perform an initial round of coin selection to estimate
	// the required fee.
	totalSat, selectedUtxos, err := selectInputs(
		amt, coins, strategy, feeRate,
	)
	if err != nil {
		return nil, 0, 0, err
	}

	// Obtain fee estimates both with and without using a change
	// output.
	requiredFeeNoChange, requiredFeeWithChange, err := calculateFees(
		selectedUtxos, feeRate, existingWeight, changeType,
	)
	if err != nil {
		return nil, 0, 0, err
	}

	// For a transaction without a change output, we'll let everything go
	// to our multi-sig output after subtracting fees.
	outputAmt := totalSat - requiredFeeNoChange
	changeAmt := btcutil.Amount(0)

	// If the output is too small after subtracting the fee, the coin
	// selection cannot be performed with an amount this small.
	if outputAmt < dustLimit {
		return nil, 0, 0, fmt.Errorf("output amount(%v) after "+
			"subtracting fees(%v) below dust limit(%v)", outputAmt,
			requiredFeeNoChange, dustLimit)
	}

	// For a transaction with a change output, everything we don't spend
	// will go to change.
	newOutput := amt - requiredFeeWithChange
	newChange := totalSat - amt

	// If adding a change output leads to both outputs being above
	// the dust limit, we'll add the change output. Otherwise we'll
	// go with the no change tx we originally found.
	if newChange >= dustLimit && newOutput >= dustLimit {
		outputAmt = newOutput
		changeAmt = newChange
	}

	// Sanity check the resulting output values to make sure we
	// don't burn a great part to fees.
	totalOut := outputAmt + changeAmt
	err = sanityCheckFee(totalOut, totalSat-totalOut, maxFeeRatio)
	if err != nil {
		return nil, 0, 0, err
	}

	return selectedUtxos, outputAmt, changeAmt, nil
}

// CoinSelectUpToAmount attempts to select coins such that we'll select up to
// maxAmount exclusive of fees and optional reserve if sufficient funds are
// available. If insufficient funds are available this method selects all
// available coins.
func CoinSelectUpToAmount(feeRate chainfee.SatPerKWeight, minAmount, maxAmount,
	reserved, dustLimit btcutil.Amount, coins []wallet.Coin,
	strategy wallet.CoinSelectionStrategy,
	existingWeight input.TxWeightEstimator,
	changeType ChangeAddressType, maxFeeRatio float64) ([]wallet.Coin,
	btcutil.Amount, btcutil.Amount, error) {

	var (
		// selectSubtractFee is tracking if our coin selection was
		// unsuccessful and whether we have to start a new round of
		// selecting coins considering fees.
		selectSubtractFee = false
		outputAmount      = maxAmount
	)

	// Get total balance from coins which we need for reserve considerations
	// and fee sanity checks.
	var totalBalance btcutil.Amount
	for _, coin := range coins {
		totalBalance += btcutil.Amount(coin.Value)
	}

	// First we try to select coins to create an output of the specified
	// maxAmount with or without a change output that covers the miner fee.
	selected, changeAmt, err := CoinSelect(
		feeRate, maxAmount, dustLimit, coins, strategy, existingWeight,
		changeType, maxFeeRatio,
	)

	var errInsufficientFunds *ErrInsufficientFunds
	switch {
	case err == nil:
		// If the coin selection succeeds we check if our total balance
		// covers the selected set of coins including fees plus an
		// optional anchor reserve.

		// First we sum up the value of all selected coins.
		var sumSelected btcutil.Amount
		for _, coin := range selected {
			sumSelected += btcutil.Amount(coin.Value)
		}

		// We then subtract the change amount from the value of all
		// selected coins to obtain the actual amount that is selected.
		sumSelected -= changeAmt

		// Next we check if our total balance can cover for the selected
		// output plus the optional anchor reserve.
		if totalBalance-sumSelected < reserved {
			// If our local balance is insufficient to cover for the
			// reserve we try to select an output amount that uses
			// our total balance minus reserve and fees.
			selectSubtractFee = true
		}

	case errors.As(err, &errInsufficientFunds):
		// If the initial coin selection fails due to insufficient funds
		// we select our total available balance minus fees.
		selectSubtractFee = true

	default:
		return nil, 0, 0, err
	}

	// If we determined that our local balance is insufficient we check
	// our total balance minus fees and optional reserve.
	if selectSubtractFee {
		selected, outputAmount, changeAmt, err = CoinSelectSubtractFees(
			feeRate, totalBalance-reserved, dustLimit, coins,
			strategy, existingWeight, changeType, maxFeeRatio,
		)
		if err != nil {
			return nil, 0, 0, err
		}
	}

	// Sanity check the resulting output values to make sure we don't burn a
	// great part to fees.
	totalOut := outputAmount + changeAmt
	sum := func(coins []wallet.Coin) btcutil.Amount {
		var sum btcutil.Amount
		for _, coin := range coins {
			sum += btcutil.Amount(coin.Value)
		}

		return sum
	}
	err = sanityCheckFee(totalOut, sum(selected)-totalOut, maxFeeRatio)
	if err != nil {
		return nil, 0, 0, err
	}

	// In case the selected amount is lower than minimum funding amount we
	// must return an error. The minimum funding amount is determined
	// upstream and denotes either the minimum viable channel size or an
	// amount sufficient to cover for the initial remote balance.
	if outputAmount < minAmount {
		return nil, 0, 0, fmt.Errorf("available funds(%v) below the "+
			"minimum amount(%v)", outputAmount, minAmount)
	}

	return selected, outputAmount, changeAmt, nil
}
