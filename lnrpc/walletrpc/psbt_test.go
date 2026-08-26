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
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// unsupportedLeaseOptionsErr is returned when a wallet cannot apply a
// requested confirmation-controlled lease.
const unsupportedLeaseOptionsErr = "wallet does not support " +
	"release-after-spend output leases"

// leaseOptionsWallet records the optional lease settings passed by lockInputs.
type leaseOptionsWallet struct {
	*mock.WalletController

	leaseCalls  []lnwallet.LeaseOutputOptions
	legacyCalls int
	releasedIDs []wtxmgr.LockID
	failCall    int
}

// legacyLeaseWallet records calls to the original lease method but does not
// implement OutputLeaserWithOptions.
type legacyLeaseWallet struct {
	*mock.WalletController

	leaseCalls int
}

// LeaseOutput records any fallback to the legacy lease path.
func (w *legacyLeaseWallet) LeaseOutput(_ wtxmgr.LockID, _ wire.OutPoint,
	_ time.Duration) (time.Time, error) {

	w.leaseCalls++

	return time.Unix(123, 0), nil
}

// LeaseOutputWithOptions records the requested behavior and optionally fails
// one call so the partial-lock rollback path can be asserted.
func (w *leaseOptionsWallet) LeaseOutputWithOptions(_ wtxmgr.LockID,
	_ wire.OutPoint, _ time.Duration,
	opts lnwallet.LeaseOutputOptions) (time.Time, error) {

	w.leaseCalls = append(w.leaseCalls, opts)
	if w.failCall > 0 && len(w.leaseCalls) == w.failCall {
		return time.Time{}, errors.New("lease failed")
	}

	return time.Unix(123, 0), nil
}

// LeaseOutput records calls to the zero-option lease path.
func (w *leaseOptionsWallet) LeaseOutput(_ wtxmgr.LockID, _ wire.OutPoint,
	_ time.Duration) (time.Time, error) {

	w.legacyCalls++

	return time.Unix(123, 0), nil
}

// ReleaseOutput records the lock ID used to roll back an acquired lease.
func (w *leaseOptionsWallet) ReleaseOutput(id wtxmgr.LockID,
	_ wire.OutPoint) error {

	w.releasedIDs = append(w.releasedIDs, id)

	return nil
}

// TestLockInputsForwardsReleaseAfterSpend verifies that FundPsbt's lease helper
// passes the requested confirmation depth to every selected input.
func TestLockInputsForwardsReleaseAfterSpend(t *testing.T) {
	t.Parallel()

	wallet := &leaseOptionsWallet{
		WalletController: &mock.WalletController{},
	}
	lockID := wtxmgr.LockID{1, 2, 3}
	outpoints := []wire.OutPoint{
		{Index: 1},
		{Index: 2},
	}

	locks, err := lockInputs(
		wallet, outpoints, &lockID, time.Hour, 6,
	)
	require.NoError(t, err)
	require.Len(t, locks, 2)
	require.Len(t, wallet.leaseCalls, 2)
	for _, opts := range wallet.leaseCalls {
		require.Equal(t, uint32(6), opts.ReleaseAfterSpendConfs)
	}
}

// TestLockInputsUsesLegacyPathForZeroDepth verifies that the zero value keeps
// the existing time-only lease path even when the wallet supports options.
func TestLockInputsUsesLegacyPathForZeroDepth(t *testing.T) {
	t.Parallel()

	wallet := &leaseOptionsWallet{
		WalletController: &mock.WalletController{},
	}

	locks, err := lockInputs(
		wallet, []wire.OutPoint{{Index: 1}}, nil, time.Hour, 0,
	)
	require.NoError(t, err)
	require.Len(t, locks, 1)
	require.Equal(t, 1, wallet.legacyCalls)
	require.Empty(t, wallet.leaseCalls)
}

// TestLockInputsRejectsUnsupportedLeaseOptions verifies an option-bearing
// FundPsbt lease fails before falling back to a time-only wallet lease.
func TestLockInputsRejectsUnsupportedLeaseOptions(t *testing.T) {
	t.Parallel()

	controller := &legacyLeaseWallet{
		WalletController: &mock.WalletController{},
	}
	wallet := &lnwallet.LightningWallet{
		WalletController: controller,
	}

	_, err := lockInputs(
		wallet, []wire.OutPoint{{Index: 1}}, nil, time.Hour, 6,
	)
	require.ErrorContains(
		t, err, unsupportedLeaseOptionsErr,
	)
	require.Zero(t, controller.leaseCalls,
		"unsupported options must not create a shorter legacy lease")
}
// TestLockInputsRejectsUnsupportedOptionsWithoutInputs verifies capability is
// checked even when FundPsbt does not need to acquire a new input lease.
func TestLockInputsRejectsUnsupportedOptionsWithoutInputs(t *testing.T) {
	t.Parallel()

	controller := &legacyLeaseWallet{
		WalletController: &mock.WalletController{},
	}
	wallet := &lnwallet.LightningWallet{
		WalletController: controller,
	}

	_, err := lockInputs(wallet, nil, nil, time.Hour, 6)
	require.ErrorContains(t, err, unsupportedLeaseOptionsErr)
	require.Zero(t, controller.leaseCalls)
}
