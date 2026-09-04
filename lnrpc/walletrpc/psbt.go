//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

const (
	defaultMaxConf = math.MaxInt32
)

var errOutputLeaseOptionsUnsupported = fmt.Errorf(
	"wallet does not support release-after-spend output leases",
)

// verifyInputsUnspent checks that all inputs are contained in the list of
// known, non-locked UTXOs given.
func verifyInputsUnspent(inputs []*wire.TxIn, utxos []*lnwallet.Utxo) error {
	// TODO(guggero): Pass in UTXOs as a map to make lookup more efficient.
	for idx, txIn := range inputs {
		found := false
		for _, u := range utxos {
			if u.OutPoint == txIn.PreviousOutPoint {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("input %d not found in list of non-"+
				"locked UTXO", idx)
		}
	}

	return nil
}

// lockInputs requests lock leases for all inputs specified in a PSBT packet
// (the passed outpoints), using either the optional custom lock ID and duration
// or the wallet's internal static lock ID with the default 10-minute duration.
func lockInputs(w lnwallet.WalletController, outpoints []wire.OutPoint,
	customLockID *wtxmgr.LockID, customLockDuration time.Duration,
	releaseAfterSpendConfs uint32) (
	[]*base.ListLeasedOutputResult, error) {

	var leaser lnwallet.OutputLeaserWithOptions
	if releaseAfterSpendConfs > 0 {
		var ok bool
		leaser, ok = lnwallet.ResolveOutputLeaser(w)
		if !ok {
			return nil, fmt.Errorf(
				"lock inputs: %w",
				errOutputLeaseOptionsUnsupported,
			)
		}
	}

	locks := make(
		[]*base.ListLeasedOutputResult, 0, len(outpoints),
	)

	for idx := range outpoints {
		lock := &base.ListLeasedOutputResult{
			LockedOutput: &wtxmgr.LockedOutput{
				Outpoint: outpoints[idx],
			},
		}

		lock.LockID = chanfunding.LndInternalLockID
		if customLockID != nil {
			lock.LockID = *customLockID
		}

		lockDuration := chanfunding.DefaultLockDuration
		if customLockDuration != 0 {
			lockDuration = customLockDuration
		}

		// Get the details about this outpoint.
		utxo, err := w.FetchOutpointInfo(&lock.Outpoint)
		if err != nil {
			cause := fmt.Errorf("fetch outpoint info: %w", err)

			return nil, rollbackInputLeases(w, locks, cause)
		}

		var expiration time.Time
		if releaseAfterSpendConfs > 0 {
			leaseOpts := lnwallet.LeaseOutputOptions{
				ReleaseAfterSpendConfs: releaseAfterSpendConfs,
			}
			expiration, err = leaser.LeaseOutputWithOptions(
				lock.LockID, lock.Outpoint, lockDuration,
				leaseOpts,
			)
		} else {
			expiration, err = w.LeaseOutput(
				lock.LockID, lock.Outpoint, lockDuration,
			)
		}
		if err != nil {
			cause := fmt.Errorf("could not lease UTXO: %w", err)

			return nil, rollbackInputLeases(w, locks, cause)
		}

		lock.Expiration = expiration
		lock.PkScript = utxo.PkScript
		lock.Value = int64(utxo.Value)
		locks = append(locks, lock)
	}

	return locks, nil
}

// rollbackInputLeases releases every lease acquired before cause interrupted
// the current FundPsbt attempt. If a release fails, the returned error includes
// its outpoint and owner ID so the caller can identify and recover the lease.
func rollbackInputLeases(w lnwallet.WalletController,
	locks []*base.ListLeasedOutputResult, cause error) error {

	var releaseErrs []error
	for _, lock := range locks {
		err := w.ReleaseOutput(lock.LockID, lock.Outpoint)
		if err == nil {
			continue
		}

		releaseErr := fmt.Errorf(
			"could not release lease %v with lock ID %x: %w",
			lock.Outpoint, lock.LockID[:], err,
		)
		log.Errorf("%v", releaseErr)
		releaseErrs = append(releaseErrs, releaseErr)
	}

	if len(releaseErrs) == 0 {
		return cause
	}

	return errors.Join(append([]error{cause}, releaseErrs...)...)
}
