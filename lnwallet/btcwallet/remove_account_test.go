package btcwallet

import (
	"errors"
	"slices"
	"testing"

	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// removeAccountWallet is a minimal fake of btcwallet's base.Interface covering
// only the calls RemoveAccount makes. The embedded interface is deliberately
// left nil so that any additional call this test does not expect panics loudly
// rather than silently returning a zero value.
type removeAccountWallet struct {
	base.Interface

	// existing maps a key scope to the account names that already exist in
	// it, which drives the name-resolution lookups.
	existing map[waddrmgr.KeyScope][]string

	// notWatchOnly holds the names of accounts the wallet owns itself.
	// Every other account resolves as watch-only, mirroring an account
	// registered through ImportAccount.
	notWatchOnly map[string]bool

	// removedScope and removedName record the arguments of the
	// RemoveAccount call so the test can assert the resolved scope was
	// forwarded unchanged.
	removedScope waddrmgr.KeyScope
	removedName  string

	// removeAccountErr, when set, is returned by RemoveAccount.
	removeAccountErr error
}

// AccountPropertiesByName reports whether the named account exists in the
// given scope, mirroring waddrmgr's not-found error so that the caller's
// waddrmgr.IsError check behaves as it does against a real wallet.
func (w *removeAccountWallet) AccountPropertiesByName(scope waddrmgr.KeyScope,
	name string) (*waddrmgr.AccountProperties, error) {

	if slices.Contains(w.existing[scope], name) {
		return &waddrmgr.AccountProperties{
			AccountName: name,
			KeyScope:    scope,
			IsWatchOnly: !w.notWatchOnly[name],
		}, nil
	}

	return nil, newAccountNotFoundError(name)
}

// RemoveAccount records the requested scope and name.
func (w *removeAccountWallet) RemoveAccount(scope waddrmgr.KeyScope,
	name string) error {

	if w.removeAccountErr != nil {
		return w.removeAccountErr
	}

	w.removedScope = scope
	w.removedName = name

	return nil
}

// TestRemoveAccount asserts the guard rails around removing an imported
// account: only watch-only, non-reserved accounts outside lnd's internal key
// scope can be removed, an ambiguous name is refused rather than guessed at,
// and the scope the name resolves to is what the removal is forwarded with.
func TestRemoveAccount(t *testing.T) {
	t.Parallel()

	const accountName = "external"

	chainScope := waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    0,
	}

	tests := []struct {
		name          string
		accountName   string
		keyScope      *waddrmgr.KeyScope
		existing      map[waddrmgr.KeyScope][]string
		notWatchOnly  map[string]bool
		expectedScope waddrmgr.KeyScope
		expectedErr   string
	}{{
		name:        "resolved across scopes and removed",
		accountName: accountName,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0084: {accountName},
		},
		expectedScope: waddrmgr.KeyScopeBIP0084,
	}, {
		name:        "removed within explicit scope",
		accountName: accountName,
		keyScope:    &waddrmgr.KeyScopeBIP0086,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0086: {accountName},
		},
		expectedScope: waddrmgr.KeyScopeBIP0086,
	}, {
		name:        "empty name rejected",
		accountName: "",
		expectedErr: "account name is required",
	}, {
		name:        "default account name reserved",
		accountName: lnwallet.DefaultAccountName,
		expectedErr: "reserved by the wallet",
	}, {
		name:        "imported account name reserved",
		accountName: waddrmgr.ImportedAddrAccountName,
		expectedErr: "reserved by the wallet",
	}, {
		name:        "missing account not found",
		accountName: accountName,
		expectedErr: "not found",
	}, {
		// A name resolving in several scopes is only possible on
		// wallets that predate the cross-scope uniqueness check, and
		// the caller has to say which one they mean.
		name:        "ambiguous name rejected",
		accountName: accountName,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0084: {accountName},
			waddrmgr.KeyScopeBIP0086: {accountName},
		},
		expectedErr: "multiple key scopes",
	}, {
		name:        "wallet-owned account rejected",
		accountName: accountName,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0086: {accountName},
		},
		notWatchOnly: map[string]bool{accountName: true},
		expectedErr:  "not a watch-only imported account",
	}, {
		name:        "internal key scope rejected",
		accountName: accountName,
		keyScope:    &chainScope,
		existing: map[waddrmgr.KeyScope][]string{
			chainScope: {accountName},
		},
		expectedErr: "internal key scope",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fake := &removeAccountWallet{
				existing:     test.existing,
				notWatchOnly: test.notWatchOnly,
			}
			w := &BtcWallet{
				wallet:        fake,
				chainKeyScope: chainScope,
			}

			props, err := w.RemoveAccount(
				test.accountName, test.keyScope,
			)

			if test.expectedErr != "" {
				require.ErrorContains(t, err, test.expectedErr)
				require.Nil(t, props)

				// A rejected request must not have reached the
				// wallet.
				require.Empty(t, fake.removedName)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.accountName, props.AccountName)
			require.True(t, props.IsWatchOnly)
			require.Equal(t, test.expectedScope, props.KeyScope)
			require.Equal(t, test.expectedScope, fake.removedScope)
			require.Equal(t, test.accountName, fake.removedName)
		})
	}
}

// TestRemoveAccountUnsupportedWallet asserts that a wallet backend without
// removal support is reported as such instead of panicking. This pins the
// behaviour lnd has while its btcwallet dependency predates RemoveAccount.
func TestRemoveAccountUnsupportedWallet(t *testing.T) {
	t.Parallel()

	w := &BtcWallet{wallet: &lookupOnlyRemoveWallet{}}

	// An explicit scope keeps resolution to a single lookup, so that the
	// capability check is the guard that fails.
	_, err := w.RemoveAccount("external", &waddrmgr.KeyScopeBIP0084)
	require.ErrorContains(t, err, "does not support removing accounts")
}

// TestRemoveAccountWalletError asserts that a failure from the underlying
// wallet is surfaced with the account name attached.
func TestRemoveAccountWalletError(t *testing.T) {
	t.Parallel()

	walletErr := errors.New("db failure")
	fake := &removeAccountWallet{
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0084: {"external"},
		},
		removeAccountErr: walletErr,
	}
	w := &BtcWallet{wallet: fake}

	_, err := w.RemoveAccount("external", nil)
	require.ErrorIs(t, err, walletErr)
	require.ErrorContains(t, err, "external")
}

// lookupOnlyRemoveWallet answers the name-resolution lookups but deliberately
// does not implement RemoveAccount, standing in for a wallet backend that
// cannot remove accounts.
type lookupOnlyRemoveWallet struct {
	base.Interface
}

// AccountPropertiesByName always reports the account as present and
// watch-only, so that resolution succeeds and the capability check is what
// fails.
func (w *lookupOnlyRemoveWallet) AccountPropertiesByName(
	scope waddrmgr.KeyScope, name string) (*waddrmgr.AccountProperties,
	error) {

	return &waddrmgr.AccountProperties{
		AccountName: name,
		KeyScope:    scope,
		IsWatchOnly: true,
	}, nil
}
