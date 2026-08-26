//go:build walletrpc
// +build walletrpc

package walletrpc

import (
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
		[]*base.ListLeasedOutputResult, len(outpoints),
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
			return nil, fmt.Errorf("fetch outpoint info: %w", err)
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
			// If we run into a problem with locking one output, we
			// should try to unlock those that we successfully
			// locked so far. If that fails as well, there's not
			// much we can do.
			for i := 0; i < idx; i++ {
				op := locks[i].Outpoint
				if err := w.ReleaseOutput(
					chanfunding.LndInternalLockID, op,
				); err != nil {
					log.Errorf("could not release the "+
						"lock on %v: %v", op, err)
				}
			}

			return nil, fmt.Errorf("could not lease a lock on "+
				"UTXO: %v", err)
		}

		lock.Expiration = expiration
		lock.PkScript = utxo.PkScript
		lock.Value = int64(utxo.Value)
		locks[idx] = lock
	}

	return locks, nil
}
