//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/stretchr/testify/require"
)

var (
	errTestFetchLeaseOutput = errors.New("injected fetch failure")
	errTestLeaseOutput      = errors.New("lease failed")
	errTestReleaseOutput    = errors.New("release failed")
)

// leaseOptionsWallet records the optional lease settings passed by lockInputs.
type leaseOptionsWallet struct {
	*mock.WalletController

	leaseCalls     []lnwallet.LeaseOutputOptions
	legacyCalls    int
	releasedIDs    []wtxmgr.LockID
	releaseErr     error
	failCall       int
	legacyFailCall int
	fetchCalls     int
	failFetchCall  int
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
		return time.Time{}, errTestLeaseOutput
	}

	return time.Unix(123, 0), nil
}

// LeaseOutput records calls to the zero-option lease path.
func (w *leaseOptionsWallet) LeaseOutput(_ wtxmgr.LockID, _ wire.OutPoint,
	_ time.Duration) (time.Time, error) {

	w.legacyCalls++
	if w.legacyFailCall > 0 && w.legacyCalls == w.legacyFailCall {
		return time.Time{}, errTestLeaseOutput
	}

	return time.Unix(123, 0), nil
}

// ReleaseOutput records the lock ID used to roll back an acquired lease.
func (w *leaseOptionsWallet) ReleaseOutput(id wtxmgr.LockID,
	_ wire.OutPoint) error {

	w.releasedIDs = append(w.releasedIDs, id)

	return w.releaseErr
}

// FetchOutpointInfo records metadata lookups and can fail one call to verify
// that lockInputs rolls back leases acquired before the lookup failed.
func (w *leaseOptionsWallet) FetchOutpointInfo(
	outpoint *wire.OutPoint) (*lnwallet.Utxo, error) {

	w.fetchCalls++
	if w.failFetchCall > 0 && w.fetchCalls == w.failFetchCall {
		return nil, errTestFetchLeaseOutput
	}

	return w.WalletController.FetchOutpointInfo(outpoint)
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
	require.ErrorIs(t, err, errOutputLeaseOptionsUnsupported)
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
	require.ErrorIs(t, err, errOutputLeaseOptionsUnsupported)
	require.Zero(t, controller.leaseCalls)
}

// TestLockInputsRollbackUsesActualLockID verifies that a later lease failure
// releases earlier inputs with the ID that acquired them.
func TestLockInputsRollbackUsesActualLockID(t *testing.T) {
	t.Parallel()

	wallet := &leaseOptionsWallet{
		WalletController: &mock.WalletController{},
		failCall:         2,
	}
	lockID := wtxmgr.LockID{9, 8, 7}
	outpoints := []wire.OutPoint{
		{Index: 1},
		{Index: 2},
	}

	_, err := lockInputs(wallet, outpoints, &lockID, time.Hour, 6)
	require.ErrorIs(t, err, errTestLeaseOutput)
	require.Equal(t, []wtxmgr.LockID{lockID}, wallet.releasedIDs)
}

// TestLockInputsLegacyRollbackUsesActualLockID verifies the zero-depth path
// releases earlier inputs with the custom ID that acquired them.
func TestLockInputsLegacyRollbackUsesActualLockID(t *testing.T) {
	t.Parallel()

	wallet := &leaseOptionsWallet{
		WalletController: &mock.WalletController{},
		legacyFailCall:   2,
	}
	lockID := wtxmgr.LockID{9, 8, 7}
	outpoints := []wire.OutPoint{
		{Index: 1},
		{Index: 2},
	}

	_, err := lockInputs(wallet, outpoints, &lockID, time.Hour, 0)
	require.ErrorIs(t, err, errTestLeaseOutput)
	require.Equal(t, []wtxmgr.LockID{lockID}, wallet.releasedIDs)
}

// TestLockInputsReportsRollbackFailure verifies that a lease which survives a
// failed rollback remains attributable by outpoint and owner ID to the caller.
func TestLockInputsReportsRollbackFailure(t *testing.T) {
	t.Parallel()

	wallet := &leaseOptionsWallet{
		WalletController: &mock.WalletController{},
		failCall:         2,
		releaseErr:       errTestReleaseOutput,
	}
	lockID := wtxmgr.LockID{9, 8, 7}
	outpoint := wire.OutPoint{Index: 1}

	_, err := lockInputs(
		wallet, []wire.OutPoint{outpoint, {Index: 2}}, &lockID,
		time.Hour, 6,
	)
	require.ErrorIs(t, err, errTestLeaseOutput)
	require.ErrorIs(t, err, errTestReleaseOutput)
	require.ErrorContains(t, err, outpoint.String())
	require.ErrorContains(t, err, hex.EncodeToString(lockID[:]))
}

// TestLockInputsRollbackOnFetchFailure verifies that a metadata failure after
// one successful lease releases that lease with the ID that acquired it.
func TestLockInputsRollbackOnFetchFailure(t *testing.T) {
	t.Parallel()

	customLockID := wtxmgr.LockID{9, 8, 7}
	testCases := []struct {
		name       string
		lockID     *wtxmgr.LockID
		expectedID wtxmgr.LockID
	}{
		{
			name:       "internal lock ID",
			expectedID: chanfunding.LndInternalLockID,
		},
		{
			name:       "custom lock ID",
			lockID:     &customLockID,
			expectedID: customLockID,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			wallet := &leaseOptionsWallet{
				WalletController: &mock.WalletController{},
				failFetchCall:    2,
			}
			outpoints := []wire.OutPoint{
				{Index: 1},
				{Index: 2},
			}

			_, err := lockInputs(
				wallet, outpoints, testCase.lockID,
				time.Hour, 6,
			)
			require.ErrorIs(t, err, errTestFetchLeaseOutput)
			require.Len(t, wallet.leaseCalls, 1)
			require.Equal(
				t, []wtxmgr.LockID{testCase.expectedID},
				wallet.releasedIDs,
			)
		})
	}
}
