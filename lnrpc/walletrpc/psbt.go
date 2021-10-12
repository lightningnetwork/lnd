//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/psbt"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	defaultMaxConf = math.MaxInt32
)

var (
	// DefaultLockDuration is the default duration used to lock outputs.
	DefaultLockDuration = 10 * time.Minute
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

// lockInputs requests a lock lease for all inputs specified in a PSBT packet
// by using the internal, static lock ID of lnd's wallet.
func lockInputs(w lnwallet.WalletController, packet *psbt.Packet) (
	[]*wtxmgr.LockedOutput, error) {

	locks := make([]*wtxmgr.LockedOutput, len(packet.UnsignedTx.TxIn))
	for idx, rawInput := range packet.UnsignedTx.TxIn {
		lock := &wtxmgr.LockedOutput{
			LockID:   LndInternalLockID,
			Outpoint: rawInput.PreviousOutPoint,
		}

		expiration, err := w.LeaseOutput(
			lock.LockID, lock.Outpoint, DefaultLockDuration,
		)
		if err != nil {
			// If we run into a problem with locking one output, we
			// should try to unlock those that we successfully
			// locked so far. If that fails as well, there's not
			// much we can do.
			for i := 0; i < idx; i++ {
				op := locks[i].Outpoint
				if err := w.ReleaseOutput(
					LndInternalLockID, op,
				); err != nil {

					log.Errorf("could not release the "+
						"lock on %v: %v", op, err)
				}
			}

			return nil, fmt.Errorf("could not lease a lock on "+
				"UTXO: %v", err)
		}

		lock.Expiration = expiration
		locks[idx] = lock
	}

	return locks, nil
}
