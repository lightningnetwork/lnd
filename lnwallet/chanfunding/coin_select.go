package chanfunding

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
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

// CoinSelect attempts to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhering to the specified fee rate. The
// specified fee rate should be expressed in sat/kw for coin selection to
// function properly.
func CoinSelect(feeRate chainfee.SatPerKWeight, amt btcutil.Amount,
	coins []Coin) ([]Coin, btcutil.Amount, error) {

	amtNeeded := amt
	for {
		// First perform an initial round of coin selection to estimate
		// the required fee.
		totalSat, selectedUtxos, err := selectInputs(amtNeeded, coins)
		if err != nil {
			return nil, 0, err
		}

		var weightEstimate input.TxWeightEstimator

		for _, utxo := range selectedUtxos {
			switch {

			case txscript.IsPayToWitnessPubKeyHash(utxo.PkScript):
				weightEstimate.AddP2WKHInput()

			case txscript.IsPayToScriptHash(utxo.PkScript):
				weightEstimate.AddNestedP2WKHInput()

			default:
				return nil, 0, fmt.Errorf("unsupported address type: %x",
					utxo.PkScript)
			}
		}

		// Channel funding multisig output is P2WSH.
		weightEstimate.AddP2WSHOutput()

		// Assume that change output is a P2WKH output.
		//
		// TODO: Handle wallets that generate non-witness change
		// addresses.
		// TODO(halseth): make coinSelect not estimate change output
		// for dust change.
		weightEstimate.AddP2WKHOutput()

		// The difference between the selected amount and the amount
		// requested will be used to pay fees, and generate a change
		// output with the remaining.
		overShootAmt := totalSat - amt

		// Based on the estimated size and fee rate, if the excess
		// amount isn't enough to pay fees, then increase the requested
		// coin amount by the estimate required fee, performing another
		// round of coin selection.
		totalWeight := int64(weightEstimate.Weight())
		requiredFee := feeRate.FeeForWeight(totalWeight)
		if overShootAmt < requiredFee {
			amtNeeded = amt + requiredFee
			continue
		}

		// If the fee is sufficient, then calculate the size of the
		// change output.
		changeAmt := overShootAmt - requiredFee

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

	var weightEstimate input.TxWeightEstimator
	for _, utxo := range selectedUtxos {
		switch {

		case txscript.IsPayToWitnessPubKeyHash(utxo.PkScript):
			weightEstimate.AddP2WKHInput()

		case txscript.IsPayToScriptHash(utxo.PkScript):
			weightEstimate.AddNestedP2WKHInput()

		default:
			return nil, 0, 0, fmt.Errorf("unsupported address "+
				"type: %x", utxo.PkScript)
		}
	}

	// Channel funding multisig output is P2WSH.
	weightEstimate.AddP2WSHOutput()

	// At this point we've got two possibilities, either create a
	// change output, or not. We'll first try without creating a
	// change output.
	//
	// Estimate the fee required for a transaction without a change
	// output.
	totalWeight := int64(weightEstimate.Weight())
	requiredFee := feeRate.FeeForWeight(totalWeight)

	// For a transaction without a change output, we'll let everything go
	// to our multi-sig output after subtracting fees.
	outputAmt := totalSat - requiredFee
	changeAmt := btcutil.Amount(0)

	// If the the output is too small after subtracting the fee, the coin
	// selection cannot be performed with an amount this small.
	if outputAmt <= dustLimit {
		return nil, 0, 0, fmt.Errorf("output amount(%v) after "+
			"subtracting fees(%v) below dust limit(%v)", outputAmt,
			requiredFee, dustLimit)
	}

	// We were able to create a transaction with no change from the
	// selected inputs. We'll remember the resulting values for
	// now, while we try to add a change output. Assume that change output
	// is a P2WKH output.
	weightEstimate.AddP2WKHOutput()

	// Now that we have added the change output, redo the fee
	// estimate.
	totalWeight = int64(weightEstimate.Weight())
	requiredFee = feeRate.FeeForWeight(totalWeight)

	// For a transaction with a change output, everything we don't spend
	// will go to change.
	newChange := totalSat - amt
	newOutput := amt - requiredFee

	// If adding a change output leads to both outputs being above
	// the dust limit, we'll add the change output. Otherwise we'll
	// go with the no change tx we originally found.
	if newChange > dustLimit && newOutput > dustLimit {
		outputAmt = newOutput
		changeAmt = newChange
	}

	// Sanity check the resulting output values to make sure we
	// don't burn a great part to fees.
	totalOut := outputAmt + changeAmt
	fee := totalSat - totalOut

	// Fail if more than 20% goes to fees.
	// TODO(halseth): smarter fee limit. Make configurable or dynamic wrt
	// total funding size?
	if fee > totalOut/5 {
		return nil, 0, 0, fmt.Errorf("fee %v on total output"+
			"value %v", fee, totalOut)
	}

	return selectedUtxos, outputAmt, changeAmt, nil
}
