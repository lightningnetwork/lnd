package sweep

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultNumBlocksEstimate is the number of blocks that we fall back
	// to issuing an estimate for if a fee pre fence doesn't specify an
	// explicit conf target or fee rate.
	defaultNumBlocksEstimate = 6
)

// FeePreference allows callers to express their time value for inclusion of a
// transaction into a block via either a confirmation target, or a fee rate.
type FeePreference struct {
	// ConfTarget if non-zero, signals a fee preference expressed in the
	// number of desired blocks between first broadcast, and confirmation.
	ConfTarget uint32

	// FeeRate if non-zero, signals a fee pre fence expressed in the fee
	// rate expressed in sat/kw for a particular transaction.
	FeeRate chainfee.SatPerKWeight
}

// String returns a human-readable string of the fee preference.
func (p FeePreference) String() string {
	if p.ConfTarget != 0 {
		return fmt.Sprintf("%v blocks", p.ConfTarget)
	}
	return p.FeeRate.String()
}

// DetermineFeePerKw will determine the fee in sat/kw that should be paid given
// an estimator, a confirmation target, and a manual value for sat/byte. A
// value is chosen based on the two free parameters as one, or both of them can
// be zero.
func DetermineFeePerKw(feeEstimator chainfee.Estimator,
	feePref FeePreference) (chainfee.SatPerKWeight, error) {

	switch {
	// If both values are set, then we'll return an error as we require a
	// strict directive.
	case feePref.FeeRate != 0 && feePref.ConfTarget != 0:
		return 0, fmt.Errorf("only FeeRate or ConfTarget should " +
			"be set for FeePreferences")

	// If the target number of confirmations is set, then we'll use that to
	// consult our fee estimator for an adequate fee.
	case feePref.ConfTarget != 0:
		feePerKw, err := feeEstimator.EstimateFeePerKW(
			uint32(feePref.ConfTarget),
		)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee "+
				"estimator: %v", err)
		}

		return feePerKw, nil

	// If a manual sat/byte fee rate is set, then we'll use that directly.
	// We'll need to convert it to sat/kw as this is what we use
	// internally.
	case feePref.FeeRate != 0:
		feePerKW := feePref.FeeRate
		if feePerKW < chainfee.FeePerKwFloor {
			log.Infof("Manual fee rate input of %d sat/kw is "+
				"too low, using %d sat/kw instead", feePerKW,
				chainfee.FeePerKwFloor)

			feePerKW = chainfee.FeePerKwFloor
		}

		return feePerKW, nil

	// Otherwise, we'll attempt a relaxed confirmation target for the
	// transaction
	default:
		feePerKw, err := feeEstimator.EstimateFeePerKW(
			defaultNumBlocksEstimate,
		)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee estimator: "+
				"%v", err)
		}

		return feePerKw, nil
	}
}

// UtxoSource is an interface that allows a caller to access a source of UTXOs
// to use when crafting sweep transactions.
type UtxoSource interface {
	// ListUnspentWitness returns all UTXOs from the source that have
	// between minConfs and maxConfs number of confirmations.
	ListUnspentWitness(minConfs, maxConfs int32) ([]*lnwallet.Utxo, error)
}

// CoinSelectionLocker is an interface that allows the caller to perform an
// operation, which is synchronized with all coin selection attempts. This can
// be used when an operation requires that all coin selection operations cease
// forward progress. Think of this as an exclusive lock on coin selection
// operations.
type CoinSelectionLocker interface {
	// WithCoinSelectLock will execute the passed function closure in a
	// synchronized manner preventing any coin selection operations from
	// proceeding while the closure is executing. This can be seen as the
	// ability to execute a function closure under an exclusive coin
	// selection lock.
	WithCoinSelectLock(func() error) error
}

// OutpointLocker allows a caller to lock/unlock an outpoint. When locked, the
// outpoints shouldn't be used for any sort of channel funding of coin
// selection. Locked outpoints are not expected to be persisted between restarts.
type OutpointLocker interface {
	// LockOutpoint locks a target outpoint, rendering it unusable for coin
	// selection.
	LockOutpoint(o wire.OutPoint)

	// UnlockOutpoint unlocks a target outpoint, allowing it to be used for
	// coin selection once again.
	UnlockOutpoint(o wire.OutPoint)
}

// WalletSweepPackage is a package that gives the caller the ability to sweep
// ALL funds from a wallet in a single transaction. We also package a function
// closure that allows one to abort the operation.
type WalletSweepPackage struct {
	// SweepTx is a fully signed, and valid transaction that is broadcast,
	// will sweep ALL confirmed coins in the wallet with a single
	// transaction.
	SweepTx *wire.MsgTx

	// CancelSweepAttempt allows the caller to cancel the sweep attempt.
	//
	// NOTE: If the sweeping transaction isn't or cannot be broadcast, then
	// this closure MUST be called, otherwise all selected utxos will be
	// unable to be used.
	CancelSweepAttempt func()
}

// CraftSweepAllTx attempts to craft a WalletSweepPackage which will allow the
// caller to sweep ALL outputs within the wallet to a single UTXO, as specified
// by the delivery address. The sweep transaction will be crafted with the
// target fee rate, and will use the utxoSource and outpointLocker as sources
// for wallet funds.
func CraftSweepAllTx(feeRate chainfee.SatPerKWeight, dustLimit btcutil.Amount,
	blockHeight uint32, deliveryAddr btcutil.Address,
	coinSelectLocker CoinSelectionLocker, utxoSource UtxoSource,
	outpointLocker OutpointLocker, feeEstimator chainfee.Estimator,
	signer input.Signer) (*WalletSweepPackage, error) {

	// TODO(roasbeef): turn off ATPL as well when available?

	var allOutputs []*lnwallet.Utxo

	// We'll make a function closure up front that allows us to unlock all
	// selected outputs to ensure that they become available again in the
	// case of an error after the outputs have been locked, but before we
	// can actually craft a sweeping transaction.
	unlockOutputs := func() {
		for _, utxo := range allOutputs {
			outpointLocker.UnlockOutpoint(utxo.OutPoint)
		}
	}

	// Next, we'll use the coinSelectLocker to ensure that no coin
	// selection takes place while we fetch and lock all outputs the wallet
	// knows of.  Otherwise, it may be possible for a new funding flow to
	// lock an output while we fetch the set of unspent witnesses.
	err := coinSelectLocker.WithCoinSelectLock(func() error {
		// Now that we can be sure that no other coin selection
		// operations are going on, we can grab a clean snapshot of the
		// current UTXO state of the wallet.
		utxos, err := utxoSource.ListUnspentWitness(
			1, math.MaxInt32,
		)
		if err != nil {
			return err
		}

		// We'll now lock each UTXO to ensure that other callers don't
		// attempt to use these UTXOs in transactions while we're
		// crafting out sweep all transaction.
		for _, utxo := range utxos {
			outpointLocker.LockOutpoint(utxo.OutPoint)
		}

		allOutputs = append(allOutputs, utxos...)

		return nil
	})
	if err != nil {
		// If we failed at all, we'll unlock any outputs selected just
		// in case we had any lingering outputs.
		unlockOutputs()

		return nil, fmt.Errorf("unable to fetch+lock wallet "+
			"utxos: %v", err)
	}

	// Now that we've locked all the potential outputs to sweep, we'll
	// assemble an input for each of them, so we can hand it off to the
	// sweeper to generate and sign a transaction for us.
	var inputsToSweep []input.Input
	for _, output := range allOutputs {
		// As we'll be signing for outputs under control of the wallet,
		// we only need to populate the output value and output script.
		// The rest of the items will be populated internally within
		// the sweeper via the witness generation function.
		signDesc := &input.SignDescriptor{
			Output: &wire.TxOut{
				PkScript: output.PkScript,
				Value:    int64(output.Value),
			},
			HashType: txscript.SigHashAll,
		}

		pkScript := output.PkScript

		// Based on the output type, we'll map it to the proper witness
		// type so we can generate the set of input scripts needed to
		// sweep the output.
		var witnessType input.WitnessType
		switch output.AddressType {

		// If this is a p2wkh output, then we'll assume it's a witness
		// key hash witness type.
		case lnwallet.WitnessPubKey:
			witnessType = input.WitnessKeyHash

		// If this is a p2sh output, then as since it's under control
		// of the wallet, we'll assume it's a nested p2sh output.
		case lnwallet.NestedWitnessPubKey:
			witnessType = input.NestedWitnessKeyHash

		// All other output types we count as unknown and will fail to
		// sweep.
		default:
			unlockOutputs()

			return nil, fmt.Errorf("unable to sweep coins, "+
				"unknown script: %x", pkScript[:])
		}

		// Now that we've constructed the items required, we'll make an
		// input which can be passed to the sweeper for ultimate
		// sweeping.
		input := input.MakeBaseInput(
			&output.OutPoint, witnessType, signDesc, 0, nil,
		)
		inputsToSweep = append(inputsToSweep, &input)
	}

	// Next, we'll convert the delivery addr to a pkScript that we can use
	// to create the sweep transaction.
	deliveryPkScript, err := txscript.PayToAddrScript(deliveryAddr)
	if err != nil {
		unlockOutputs()

		return nil, err
	}

	// Finally, we'll ask the sweeper to craft a sweep transaction which
	// respects our fee preference and targets all the UTXOs of the wallet.
	sweepTx, err := createSweepTx(
		inputsToSweep, deliveryPkScript, blockHeight, feeRate,
		dustLimit, signer,
	)
	if err != nil {
		unlockOutputs()

		return nil, err
	}

	return &WalletSweepPackage{
		SweepTx:            sweepTx,
		CancelSweepAttempt: unlockOutputs,
	}, nil
}
