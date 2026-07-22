//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

type mockLockingWallet struct {
	*mock.WalletController

	leaseCount        int
	releasedLockIDs   []wtxmgr.LockID
	releasedOutpoints []wire.OutPoint
}

func (m *mockLockingWallet) LeaseOutput(wtxmgr.LockID, wire.OutPoint,
	time.Duration) (time.Time, error) {

	m.leaseCount++
	if m.leaseCount == 2 {
		return time.Time{}, errors.New("lease failed")
	}

	return time.Now(), nil
}

func (m *mockLockingWallet) ReleaseOutput(lockID wtxmgr.LockID,
	outpoint wire.OutPoint) error {

	m.releasedLockIDs = append(m.releasedLockIDs, lockID)
	m.releasedOutpoints = append(m.releasedOutpoints, outpoint)

	return nil
}

// TestLockInputsRollbackCustomLockID tests that a partial lease failure rolls
// back previously leased outputs using the custom lock ID.
func TestLockInputsRollbackCustomLockID(t *testing.T) {
	t.Parallel()

	wallet := &mockLockingWallet{
		WalletController: &mock.WalletController{},
	}
	customLockID := wtxmgr.LockID{1, 2, 3}
	outpoints := []wire.OutPoint{
		{Index: 1},
		{Index: 2},
	}

	locks, err := lockInputs(
		wallet, outpoints, &customLockID, time.Minute,
	)
	require.ErrorContains(t, err, "could not lease a lock on UTXO")
	require.Nil(t, locks)
	require.Equal(t, []wtxmgr.LockID{customLockID}, wallet.releasedLockIDs)
	require.Equal(t, outpoints[:1], wallet.releasedOutpoints)
}
