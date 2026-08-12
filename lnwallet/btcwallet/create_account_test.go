package btcwallet

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// createAccountWallet is a minimal fake of btcwallet's base.Interface covering
// only the calls CreateAccount makes. The embedded interface is deliberately
// left nil so that any additional call this test does not expect panics loudly
// rather than silently returning a zero value.
type createAccountWallet struct {
	base.Interface

	// existing maps a key scope to the account names that already exist in
	// it, which drives the duplicate-name lookups.
	existing map[waddrmgr.KeyScope][]string

	// createdScope and createdName record the arguments of the NextAccount
	// call so the test can assert the scope was forwarded unchanged.
	createdScope waddrmgr.KeyScope
	createdName  string

	// nextAccountErr, when set, is returned by NextAccount.
	nextAccountErr error
}

// AccountPropertiesByName reports whether the named account exists in the given
// scope, mirroring waddrmgr's not-found error so that the caller's
// waddrmgr.IsError check behaves as it does against a real wallet.
func (w *createAccountWallet) AccountPropertiesByName(scope waddrmgr.KeyScope,
	name string) (*waddrmgr.AccountProperties, error) {

	if slices.Contains(w.existing[scope], name) {
		return &waddrmgr.AccountProperties{
			AccountName: name,
		}, nil
	}

	return nil, newAccountNotFoundError(name)
}

// lookupOnlyWallet answers the duplicate-name lookups but deliberately does not
// implement NextAccount, standing in for a wallet backend that cannot derive
// new accounts.
type lookupOnlyWallet struct {
	base.Interface
}

// AccountPropertiesByName always reports the account as missing.
func (w *lookupOnlyWallet) AccountPropertiesByName(_ waddrmgr.KeyScope,
	name string) (*waddrmgr.AccountProperties, error) {

	return nil, newAccountNotFoundError(name)
}

// NextAccount records the requested scope and name.
func (w *createAccountWallet) NextAccount(scope waddrmgr.KeyScope,
	name string) (uint32, error) {

	if w.nextAccountErr != nil {
		return 0, w.nextAccountErr
	}

	w.createdScope = scope
	w.createdName = name

	return 7, nil
}

// AccountProperties returns the properties of the freshly created account.
func (w *createAccountWallet) AccountProperties(_ waddrmgr.KeyScope,
	account uint32) (*waddrmgr.AccountProperties, error) {

	return &waddrmgr.AccountProperties{
		AccountNumber: account,
		AccountName:   w.createdName,
	}, nil
}

// TestCreateAccount asserts the guard rails around creating a wallet-owned
// account: the wallet's own reserved account names cannot be taken, a name may
// not be reused, and the requested key scope is what the account is created in.
func TestCreateAccount(t *testing.T) {
	t.Parallel()

	const accountName = "custom"

	tests := []struct {
		name        string
		accountName string
		keyScope    waddrmgr.KeyScope
		existing    map[waddrmgr.KeyScope][]string
		expectedErr string
	}{{
		name:        "taproot account created",
		accountName: accountName,
		keyScope:    waddrmgr.KeyScopeBIP0086,
	}, {
		name:        "witness pubkey account created",
		accountName: accountName,
		keyScope:    waddrmgr.KeyScopeBIP0084,
	}, {
		name:        "empty name rejected",
		accountName: "",
		keyScope:    waddrmgr.KeyScopeBIP0086,
		expectedErr: "account name is required",
	}, {
		name:        "default account name reserved",
		accountName: lnwallet.DefaultAccountName,
		keyScope:    waddrmgr.KeyScopeBIP0086,
		expectedErr: "reserved by the wallet",
	}, {
		name:        "imported account name reserved",
		accountName: waddrmgr.ImportedAddrAccountName,
		keyScope:    waddrmgr.KeyScopeBIP0086,
		expectedErr: "reserved by the wallet",
	}, {
		name:        "duplicate in requested scope rejected",
		accountName: accountName,
		keyScope:    waddrmgr.KeyScopeBIP0086,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0086: {accountName},
		},
		expectedErr: "already exists",
	}, {
		// A name that exists under a different scope must also be
		// rejected: coin selection resolves a custom account name to
		// whichever scope matches first, so allowing the same name
		// twice would make later funding calls ambiguous.
		name:        "duplicate in other scope rejected",
		accountName: accountName,
		keyScope:    waddrmgr.KeyScopeBIP0086,
		existing: map[waddrmgr.KeyScope][]string{
			waddrmgr.KeyScopeBIP0084: {accountName},
		},
		expectedErr: "already exists",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fake := &createAccountWallet{existing: test.existing}
			w := &BtcWallet{wallet: fake}

			props, err := w.CreateAccount(
				test.keyScope, test.accountName,
			)

			if test.expectedErr != "" {
				require.ErrorContains(t, err, test.expectedErr)
				require.Nil(t, props)

				// A rejected request must not have reached the
				// wallet.
				require.Empty(t, fake.createdName)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.accountName, props.AccountName)
			require.Equal(t, test.keyScope, fake.createdScope)
			require.Equal(t, test.accountName, fake.createdName)
		})
	}
}

// TestCreateAccountUnsupportedWallet asserts that a wallet backend which cannot
// derive new accounts is reported as such instead of panicking.
func TestCreateAccountUnsupportedWallet(t *testing.T) {
	t.Parallel()

	w := &BtcWallet{wallet: &lookupOnlyWallet{}}

	_, err := w.CreateAccount(waddrmgr.KeyScopeBIP0086, "custom")
	require.ErrorContains(t, err, "does not support creating accounts")
}

// TestCreateAccountWalletError asserts that a failure from the underlying
// wallet is surfaced with the account name attached.
func TestCreateAccountWalletError(t *testing.T) {
	t.Parallel()

	walletErr := errors.New("wallet is locked")
	fake := &createAccountWallet{nextAccountErr: walletErr}
	w := &BtcWallet{wallet: fake}

	_, err := w.CreateAccount(waddrmgr.KeyScopeBIP0086, "custom")
	require.ErrorIs(t, err, walletErr)
	require.ErrorContains(t, err, "custom")
}

// TestCreateAccountSerialisesCallers asserts that concurrent CreateAccount
// calls do not overlap.
//
// The duplicate-name check and the creation are separate database
// transactions, and btcwallet's own check is per-scope, so two overlapping
// calls could both pass the check and both create — leaving one name in two
// key scopes, the exact ambiguity the check exists to prevent. The wallet
// fake reports the greatest number of calls it ever saw inside the critical
// section, which is 1 only while the caller serialises them.
func TestCreateAccountSerialisesCallers(t *testing.T) {
	t.Parallel()

	const callers = 8

	fake := &serialisingWallet{}
	w := &BtcWallet{wallet: fake}

	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Distinct names: a shared name would be rejected by
			// the duplicate check, which is a different property
			// from the one under test here.
			_, _ = w.CreateAccount(
				waddrmgr.KeyScopeBIP0086,
				fmt.Sprintf("custom-%d", i),
			)
		}()
	}
	wg.Wait()

	require.Equal(t, 1, fake.maxInFlight(),
		"CreateAccount calls must not overlap")
	require.Equal(t, callers, fake.created)
}

// serialisingWallet records created accounts and tracks how many callers are
// ever inside CreateAccount's check-then-create section at once.
type serialisingWallet struct {
	base.Interface

	mtx      sync.Mutex
	names    []string
	created  int
	inFlight int
	maxSeen  int
}

// AccountPropertiesByName marks the caller as in-flight, pauses long enough
// for any unsynchronised peer to overlap with it, and reports whether the
// account exists.
func (w *serialisingWallet) AccountPropertiesByName(_ waddrmgr.KeyScope,
	name string) (*waddrmgr.AccountProperties, error) {

	w.enter()
	defer w.exit()

	time.Sleep(time.Millisecond)

	w.mtx.Lock()
	defer w.mtx.Unlock()

	if slices.Contains(w.names, name) {
		return &waddrmgr.AccountProperties{AccountName: name}, nil
	}

	return nil, newAccountNotFoundError(name)
}

// enter records one more caller inside the section.
func (w *serialisingWallet) enter() {
	w.mtx.Lock()
	defer w.mtx.Unlock()

	w.inFlight++
	if w.inFlight > w.maxSeen {
		w.maxSeen = w.inFlight
	}
}

// exit records one fewer caller inside the section.
func (w *serialisingWallet) exit() {
	w.mtx.Lock()
	defer w.mtx.Unlock()

	w.inFlight--
}

// maxInFlight reports the greatest overlap observed.
func (w *serialisingWallet) maxInFlight() int {
	w.mtx.Lock()
	defer w.mtx.Unlock()

	return w.maxSeen
}

// NextAccount records the new account. It is instrumented like the lookup so
// the test observes the whole check-then-create section: a lock around only
// the lookup would otherwise pass while leaving the real window open.
func (w *serialisingWallet) NextAccount(_ waddrmgr.KeyScope,
	name string) (uint32, error) {

	w.enter()
	defer w.exit()

	time.Sleep(time.Millisecond)

	w.mtx.Lock()
	defer w.mtx.Unlock()

	w.names = append(w.names, name)
	w.created++

	return uint32(w.created), nil
}

// AccountProperties returns the properties of the created account.
func (w *serialisingWallet) AccountProperties(_ waddrmgr.KeyScope,
	account uint32) (*waddrmgr.AccountProperties, error) {

	w.mtx.Lock()
	defer w.mtx.Unlock()

	return &waddrmgr.AccountProperties{
		AccountNumber: account,
		AccountName:   w.names[len(w.names)-1],
	}, nil
}
