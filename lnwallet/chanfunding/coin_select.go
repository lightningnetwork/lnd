package chanfunding

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ErrInsufficientFunds is a type matching the error interface which is
// returned when coin selection for a new funding transaction fails to due
// having an insufficient amount of confirmed funds.
type ErrInsufficientFunds struct {
	amountAvailable btcutil.Amount
	amountSelected  btcutil.Amount
}

// Error returns a human readable string describing the error.
func (e *ErrInsufficientFunds) Error() string {
	return fmt.Sprintf("not enough witness outputs to create funding "+
		"transaction, need %v only have %v  available",
		e.amountAvailable, e.amountSelected)
}

// errUnsupportedInput is a type matching the error interface, which is returned
// when trying to calculate the fee of a transaction that references an
// unsupported script in the outpoint of a transaction input.
type errUnsupportedInput struct {
	PkScript []byte
}

// Error returns a human readable string describing the error.
func (e *errUnsupportedInput) Error() string {
	return fmt.Sprintf("unsupported address type: %x", e.PkScript)
}

// Coin represents a spendable UTXO which is available for channel funding.
// This UTXO need not reside in our internal wallet as an example, and instead
// may be derived from an existing watch-only wallet. It wraps both the output
// present within the UTXO set, and also the outpoint that generates this coin.
type Coin struct {
	wire.TxOut

	wire.OutPoint
}

// selectInputs selects a slice of inputs necessary to meet the specified
// selection amount. If input selection is unable to succeed due to insufficient
// funds, a non-nil error is returned. Additionally, the total amount of the
// selected coins are returned in order for the caller to properly handle
// change+fees.
func selectInputs(amt btcutil.Amount, coins []Coin) (btcutil.Amount, []Coin, error) {

	satSelected := btcutil.Amount(0)
	for i, coin := range coins {
		satSelected += btcutil.Amount(coin.Value)
		if satSelected >= amt {
			return satSelected, coins[:i+1], nil
		}
	}

	return 0, nil, &ErrInsufficientFunds{amt, satSelected}
}

// calculateFees returns for the specified utxos and fee rate two fee
// estimates, one calculated using a change output and one without. The weight
// added to the estimator from a change output is for a P2WKH output.
func calculateFees(utxos []Coin, feeRate chainfee.SatPerKWeight) (btcutil.Amount,
	btcutil.Amount, error) {

	var weightEstimate input.TxWeightEstimator
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

	// Channel funding multisig output is P2WSH.
	weightEstimate.AddP2WSHOutput()

	// Estimate the fee required for a transaction without a change
	// output.
	totalWeight := int64(weightEstimate.Weight())
	requiredFeeNoChange := feeRate.FeeForWeight(totalWeight)

	// Estimate the fee required for a transaction with a change output.
	// Assume that change output is a P2TR output.
	weightEstimate.AddP2TROutput()

	// Now that we have added the change output, redo the fee
	// estimate.
	totalWeight = int64(weightEstimate.Weight())
	requiredFeeWithChange := feeRate.FeeForWeight(totalWeight)

	return requiredFeeNoChange, requiredFeeWithChange, nil
}

// sanityCheckFee checks if the specified fee amounts to over 20% of the total
// output amount and raises an error.
func sanityCheckFee(totalOut, fee btcutil.Amount) error {
	// Fail if more than 20% goes to fees.
	// TODO(halseth): smarter fee limit. Make configurable or dynamic wrt
	// total funding size?
	if fee > totalOut/5 {
		return fmt.Errorf("fee %v on total output value %v", fee,
			totalOut)
	}
	return nil
}

// CoinSelect attempts to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhering to the specified fee rate. The
// specified fee rate should be expressed in sat/kw for coin selection to
// function properly.
func CoinSelect(feeRate chainfee.SatPerKWeight, amt, dustLimit btcutil.Amount,
	coins []Coin) ([]Coin, btcutil.Amount, error) {

	amtNeeded := amt
	for {
		// First perform an initial round of coin selection to estimate
		// the required fee.
		totalSat, selectedUtxos, err := selectInputs(amtNeeded, coins)
		if err != nil {
			return nil, 0, err
		}

		// Obtain fee estimates both with and without using a change
		// output.
		requiredFeeNoChange, requiredFeeWithChange, err := calculateFees(
			selectedUtxos, feeRate,
		)
		if err != nil {
			return nil, 0, err
		}

		// The difference between the selected amount and the amount
		// requested will be used to pay fees, and generate a change
		// output with the remaining.
		overShootAmt := totalSat - amt

		var changeAmt btcutil.Amount

		switch {
		// If the excess amount isn't enough to pay for fees based on
		// fee rate and estimated size without using a change output,
		// then increase the requested coin amount by the estimate
		// required fee without using change, performing another round
		// of coin selection.
		case overShootAmt < requiredFeeNoChange:
			amtNeeded = amt + requiredFeeNoChange
			continue

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

		if changeAmt < dustLimit {
			changeAmt = 0
		}

		// Sanity check the resulting output values to make sure we
		// don't burn a great part to fees.
		totalOut := amt + changeAmt
		err = sanityCheckFee(totalOut, totalSat-totalOut)
		if err != nil {
			return nil, 0, err
		}

		return selectedUtxos, changeAmt, nil
	}
}

// CoinSelectSubtractFees attempts to select coins such that we'll spend up to
// amt in total after fees, adhering to the specified fee rate. The selected
// coins, the final output and change values are returned.
func CoinSelectSubtractFees(feeRate chainfee.SatPerKWeight, amt,
	dustLimit btcutil.Amount, coins []Coin) ([]Coin, btcutil.Amount,
	btcutil.Amount, error) {

	// First perform an initial round of coin selection to estimate
	// the required fee.
	totalSat, selectedUtxos, err := selectInputs(amt, coins)
	if err != nil {
		return nil, 0, 0, err
	}

	// Obtain fee estimates both with and without using a change
	// output.
	requiredFeeNoChange, requiredFeeWithChange, err := calculateFees(
		selectedUtxos, feeRate,
	)
	if err != nil {
		return nil, 0, 0, err
	}

	// For a transaction without a change output, we'll let everything go
	// to our multi-sig output after subtracting fees.
	outputAmt := totalSat - requiredFeeNoChange
	changeAmt := btcutil.Amount(0)

	// If the the output is too small after subtracting the fee, the coin
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
	err = sanityCheckFee(totalOut, totalSat-totalOut)
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
	reserved, dustLimit btcutil.Amount, coins []Coin) ([]Coin,
	btcutil.Amount, btcutil.Amount, error) {

	var (
		// selectSubtractFee is tracking if our coin selection was
		// unsuccessful and whether we have to start a new round of
		// selecting coins considering fees.
		selectSubtractFee = false
		outputAmount      = maxAmount
	)

	// Get total balance from coins which we need for reserve considerations
	// and fee santiy checks.
	var totalBalance btcutil.Amount
	for _, coin := range coins {
		totalBalance += btcutil.Amount(coin.Value)
	}

	// First we try to select coins to create an output of the specified
	// maxAmount with or without a change output that covers the miner fee.
	selected, changeAmt, err := CoinSelect(
		feeRate, maxAmount, dustLimit, coins,
	)

	var errInsufficientFunds *ErrInsufficientFunds
	if err == nil { //nolint:gocritic,ifElseChain
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
	} else if errors.As(err, &errInsufficientFunds) {
		// If the initial coin selection fails due to insufficient funds
		// we select our total available balance minus fees.
		selectSubtractFee = true
	} else {
		return nil, 0, 0, err
	}

	// If we determined that our local balance is insufficient we check
	// our total balance minus fees and optional reserve.
	if selectSubtractFee {
		selected, outputAmount, changeAmt, err = CoinSelectSubtractFees(
			feeRate, totalBalance-reserved, dustLimit, coins,
		)
		if err != nil {
			return nil, 0, 0, err
		}
	}

	// Sanity check the resulting output values to make sure we don't burn a
	// great part to fees.
	totalOut := outputAmount + changeAmt
	sum := func(coins []Coin) btcutil.Amount {
		var sum btcutil.Amount
		for _, coin := range coins {
			sum += btcutil.Amount(coin.Value)
		}

		return sum
	}
	err = sanityCheckFee(totalOut, sum(selected)-totalOut)
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
