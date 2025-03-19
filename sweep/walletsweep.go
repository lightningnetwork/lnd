package sweep

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

var (
	// ErrNoFeePreference is returned when we attempt to satisfy a sweep
	// request from a client whom did not specify a fee preference.
	ErrNoFeePreference = errors.New("no fee preference specified")

	// ErrFeePreferenceConflict is returned when both a fee rate and a conf
	// target is set for a fee preference.
	ErrFeePreferenceConflict = errors.New("fee preference conflict")

	// ErrUnknownUTXO is returned when creating a sweeping tx using an UTXO
	// that's unknown to the wallet.
	ErrUnknownUTXO = errors.New("unknown utxo")
)

// FeePreference defines an interface that allows the caller to specify how the
// fee rate should be handled. Depending on the implementation, the fee rate
// can either be specified directly, or via a conf target which relies on the
// chain backend(`bitcoind`) to give a fee estimation, or a customized fee
// function which handles fee calculation based on the specified
// urgency(deadline).
type FeePreference interface {
	// String returns a human-readable string of the fee preference.
	String() string

	// Estimate takes a fee estimator and a max allowed fee rate and
	// returns a fee rate for the given fee preference. It ensures that the
	// fee rate respects the bounds of the relay fee and the specified max
	// fee rates.
	Estimate(chainfee.Estimator,
		chainfee.SatPerKWeight) (chainfee.SatPerKWeight, error)
}

// FeeEstimateInfo allows callers to express their time value for inclusion of
// a transaction into a block via either a confirmation target, or a fee rate.
type FeeEstimateInfo struct {
	// ConfTarget if non-zero, signals a fee preference expressed in the
	// number of desired blocks between first broadcast, and confirmation.
	ConfTarget uint32

	// FeeRate if non-zero, signals a fee pre fence expressed in the fee
	// rate expressed in sat/kw for a particular transaction.
	FeeRate chainfee.SatPerKWeight
}

// Compile-time constraint to ensure FeeEstimateInfo implements FeePreference.
var _ FeePreference = (*FeeEstimateInfo)(nil)

// String returns a human-readable string of the fee preference.
func (f FeeEstimateInfo) String() string {
	if f.ConfTarget != 0 {
		return fmt.Sprintf("%v blocks", f.ConfTarget)
	}

	return f.FeeRate.String()
}

// Estimate returns a fee rate for the given fee preference. It ensures that
// the fee rate respects the bounds of the relay fee and the max fee rates, if
// specified.
func (f FeeEstimateInfo) Estimate(estimator chainfee.Estimator,
	maxFeeRate chainfee.SatPerKWeight) (chainfee.SatPerKWeight, error) {

	var (
		feeRate chainfee.SatPerKWeight
		err     error
	)

	switch {
	// Ensure a type of fee preference is specified to prevent using a
	// default below.
	case f.FeeRate == 0 && f.ConfTarget == 0:
		return 0, ErrNoFeePreference

	// If both values are set, then we'll return an error as we require a
	// strict directive.
	case f.FeeRate != 0 && f.ConfTarget != 0:
		return 0, ErrFeePreferenceConflict

	// If the target number of confirmations is set, then we'll use that to
	// consult our fee estimator for an adequate fee.
	case f.ConfTarget != 0:
		feeRate, err = estimator.EstimateFeePerKW((f.ConfTarget))
		if err != nil {
			return 0, fmt.Errorf("unable to query fee "+
				"estimator: %w", err)
		}

	// If a manual sat/kw fee rate is set, then we'll use that directly.
	// We'll need to convert it to sat/kw as this is what we use
	// internally.
	case f.FeeRate != 0:
		feeRate = f.FeeRate

		// Because the user can specify 1 sat/vByte on the RPC
		// interface, which corresponds to 250 sat/kw, we need to bump
		// that to the minimum "safe" fee rate which is 253 sat/kw.
		if feeRate == chainfee.AbsoluteFeePerKwFloor {
			log.Infof("Manual fee rate input of %d sat/kw is "+
				"too low, using %d sat/kw instead", feeRate,
				chainfee.FeePerKwFloor)

			feeRate = chainfee.FeePerKwFloor
		}
	}

	// Get the relay fee as the min fee rate.
	minFeeRate := estimator.RelayFeePerKW()

	// If that bumped fee rate of at least 253 sat/kw is still lower than
	// the relay fee rate, we return an error to let the user know. Note
	// that "Relay fee rate" may mean slightly different things depending
	// on the backend. For bitcoind, it is effectively max(relay fee, min
	// mempool fee).
	if feeRate < minFeeRate {
		return 0, fmt.Errorf("%w: got %v, minimum is %v",
			ErrFeePreferenceTooLow, feeRate, minFeeRate)
	}

	// If a maxFeeRate is specified and the estimated fee rate is above the
	// maximum allowed fee rate, default to the max fee rate.
	if maxFeeRate != 0 && feeRate > maxFeeRate {
		log.Warnf("Estimated fee rate %v exceeds max allowed fee "+
			"rate %v, using max fee rate instead", feeRate,
			maxFeeRate)

		return maxFeeRate, nil
	}

	return feeRate, nil
}

// UtxoSource is an interface that allows a caller to access a source of UTXOs
// to use when crafting sweep transactions.
type UtxoSource interface {
	// ListUnspentWitnessFromDefaultAccount returns all UTXOs from the
	// default wallet account that have between minConfs and maxConfs
	// number of confirmations.
	ListUnspentWitnessFromDefaultAccount(minConfs, maxConfs int32) (
		[]*lnwallet.Utxo, error)
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

// OutputLeaser allows a caller to lease/release an output. When leased, the
// outputs shouldn't be used for any sort of channel funding or coin selection.
// Leased outputs are expected to be persisted between restarts.
type OutputLeaser interface {
	// LeaseOutput leases a target output, rendering it unusable for coin
	// selection.
	LeaseOutput(i wtxmgr.LockID, o wire.OutPoint, d time.Duration) (
		time.Time, error)

	// ReleaseOutput releases a target output, allowing it to be used for
	// coin selection once again.
	ReleaseOutput(i wtxmgr.LockID, o wire.OutPoint) error
}

// WalletSweepPackage is a package that gives the caller the ability to sweep
// relevant funds from a wallet in a single transaction. We also package a
// function closure that allows one to abort the operation.
type WalletSweepPackage struct {
	// SweepTx is a fully signed, and valid transaction that is broadcast,
	// will sweep ALL relevant confirmed coins in the wallet with a single
	// transaction.
	SweepTx *wire.MsgTx

	// CancelSweepAttempt allows the caller to cancel the sweep attempt.
	//
	// NOTE: If the sweeping transaction isn't or cannot be broadcast, then
	// this closure MUST be called, otherwise all selected utxos will be
	// unable to be used.
	CancelSweepAttempt func()
}

// DeliveryAddr is a pair of (address, amount) used to craft a transaction
// paying to more than one specified address.
type DeliveryAddr struct {
	// Addr is the address to pay to.
	Addr btcutil.Address

	// Amt is the amount to pay to the given address.
	Amt btcutil.Amount
}

// CraftSweepAllTx attempts to craft a WalletSweepPackage which will allow the
// caller to sweep ALL funds in ALL or SELECT outputs within the wallet to a
// list of outputs. Any leftover amount after these outputs and transaction fee,
// is sent to a single output, as specified by the change address. The sweep
// transaction will be crafted with the target fee rate, and will use the
// utxoSource and outputLeaser as sources for wallet funds.
func CraftSweepAllTx(feeRate, maxFeeRate chainfee.SatPerKWeight,
	blockHeight uint32, deliveryAddrs []DeliveryAddr,
	changeAddr btcutil.Address, coinSelectLocker CoinSelectionLocker,
	utxoSource UtxoSource, outputLeaser OutputLeaser,
	signer input.Signer, minConfs int32,
	selectUtxos fn.Set[wire.OutPoint]) (*WalletSweepPackage, error) {

	// TODO(roasbeef): turn off ATPL as well when available?

	var outputsForSweep []*lnwallet.Utxo

	// We'll make a function closure up front that allows us to unlock all
	// selected outputs to ensure that they become available again in the
	// case of an error after the outputs have been locked, but before we
	// can actually craft a sweeping transaction.
	unlockOutputs := func() {
		for _, utxo := range outputsForSweep {
			// Log the error but continue since we're already
			// handling an error.
			err := outputLeaser.ReleaseOutput(
				chanfunding.LndInternalLockID, utxo.OutPoint,
			)
			if err != nil {
				log.Warnf("Failed to release UTXO %s (%v))",
					utxo.OutPoint, err)
			}
		}
	}

	// Next, we'll use the coinSelectLocker to ensure that no coin
	// selection takes place while we fetch and lock outputs in the
	// wallet. Otherwise, it may be possible for a new funding flow to lock
	// an output while we fetch the set of unspent witnesses.
	err := coinSelectLocker.WithCoinSelectLock(func() error {
		log.Trace("[WithCoinSelectLock] entered the lock")

		// Now that we can be sure that no other coin selection
		// operations are going on, we can grab a clean snapshot of the
		// current UTXO state of the wallet.
		utxos, err := utxoSource.ListUnspentWitnessFromDefaultAccount(
			minConfs, math.MaxInt32,
		)
		if err != nil {
			return err
		}

		log.Trace("[WithCoinSelectLock] finished fetching UTXOs")

		// Use select utxos, if provided.
		if len(selectUtxos) > 0 {
			utxos, err = fetchUtxosFromOutpoints(
				utxos, selectUtxos.ToSlice(),
			)
			if err != nil {
				return err
			}
		}

		// We'll now lock each UTXO to ensure that other callers don't
		// attempt to use these UTXOs in transactions while we're
		// crafting out sweep all transaction.
		for _, utxo := range utxos {
			log.Tracef("[WithCoinSelectLock] leasing utxo: %v",
				utxo.OutPoint)

			_, err = outputLeaser.LeaseOutput(
				chanfunding.LndInternalLockID, utxo.OutPoint,
				chanfunding.DefaultLockDuration,
			)
			if err != nil {
				return err
			}
		}

		log.Trace("[WithCoinSelectLock] exited the lock")

		outputsForSweep = append(outputsForSweep, utxos...)

		return nil
	})
	if err != nil {
		// If we failed at all, we'll unlock any outputs selected just
		// in case we had any lingering outputs.
		unlockOutputs()

		return nil, fmt.Errorf("unable to fetch+lock wallet utxos: %w",
			err)
	}

	// Now that we've locked all the potential outputs to sweep, we'll
	// assemble an input for each of them, so we can hand it off to the
	// sweeper to generate and sign a transaction for us.
	var inputsToSweep []input.Input
	for _, output := range outputsForSweep {
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

		case lnwallet.TaprootPubkey:
			witnessType = input.TaprootPubKeySpend
			signDesc.HashType = txscript.SigHashDefault

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

	// Create a list of TxOuts from the given delivery addresses.
	var txOuts []*wire.TxOut
	for _, d := range deliveryAddrs {
		pkScript, err := txscript.PayToAddrScript(d.Addr)
		if err != nil {
			unlockOutputs()

			return nil, err
		}

		txOuts = append(txOuts, &wire.TxOut{
			PkScript: pkScript,
			Value:    int64(d.Amt),
		})
	}

	// Next, we'll convert the change addr to a pkScript that we can use
	// to create the sweep transaction.
	changePkScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		unlockOutputs()

		return nil, err
	}

	// Finally, we'll ask the sweeper to craft a sweep transaction which
	// respects our fee preference and targets all the UTXOs of the wallet.
	sweepTx, _, err := createSweepTx(
		inputsToSweep, txOuts, changePkScript, blockHeight,
		feeRate, maxFeeRate, signer,
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

// fetchUtxosFromOutpoints returns UTXOs for given outpoints. Errors if any
// outpoint is not in the passed slice of utxos.
func fetchUtxosFromOutpoints(utxos []*lnwallet.Utxo,
	outpoints []wire.OutPoint) ([]*lnwallet.Utxo, error) {

	lookup := fn.SliceToMap(utxos, func(utxo *lnwallet.Utxo) wire.OutPoint {
		return utxo.OutPoint
	}, func(utxo *lnwallet.Utxo) *lnwallet.Utxo {
		return utxo
	})

	subMap, err := fn.NewSubMap(lookup, outpoints)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknownUTXO, err.Error())
	}

	fetchedUtxos := slices.Collect(maps.Values(subMap))

	return fetchedUtxos, nil
}
