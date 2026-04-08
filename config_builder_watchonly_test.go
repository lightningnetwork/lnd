package lnd

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/stretchr/testify/require"
)

// TestImportWatchOnlyAccountsPreservesSparseAccountNumbers ensures watch-only
// initialization preserves explicit sparse account indices so deriving keys by
// KeyFamily uses the intended internal account number.
func TestImportWatchOnlyAccountsPreservesSparseAccountNumbers(t *testing.T) {
	const (
		pubPass           = "public-pass"
		sparseAccount     = uint32(43210)
		masterFingerprint = uint32(0x12345678)
	)

	loader := wallet.NewLoader(
		&chaincfg.TestNet3Params, t.TempDir(), true,
		kvdb.DefaultDBTimeout, 0,
	)
	testWallet, err := loader.CreateNewWatchingOnlyWallet(
		[]byte(pubPass), time.Unix(0, 0),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, loader.UnloadWallet())
	})

	rootKey, err := hdkeychain.NewMaster(
		[]byte("01234567890123456789012345678901"),
		&chaincfg.TestNet3Params,
	)
	require.NoError(t, err)

	bip84AccountXpub, err := deriveAccountXpub(
		rootKey, waddrmgr.KeyScopeBIP0084.Purpose,
		waddrmgr.KeyScopeBIP0084.Coin, 0,
	)
	require.NoError(t, err)

	customScope := waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    keychain.CoinTypeTestnet,
	}
	customAccountXpub, err := deriveAccountXpub(
		rootKey, customScope.Purpose, customScope.Coin, sparseAccount,
	)
	require.NoError(t, err)

	watchOnlyAccounts := map[waddrmgr.ScopedIndex]*hdkeychain.ExtendedKey{
		{
			Scope: waddrmgr.KeyScopeBIP0084,
			Index: 0,
		}: bip84AccountXpub,
		{
			Scope: customScope,
			Index: sparseAccount,
		}: customAccountXpub,
	}
	initMsg := &walletunlocker.WalletInitMsg{
		WatchOnlyMasterFingerprint: masterFingerprint,
		WatchOnlyAccounts:          watchOnlyAccounts,
	}
	require.NoError(t, importWatchOnlyAccounts(testWallet, initMsg))

	// This is the same call path WalletKit.DeriveKey uses for watch-only
	// wallets: no account auto-creation, account number must already exist.
	keyRing := keychain.NewBtcWalletKeyRing(
		testWallet, keychain.CoinTypeTestnet,
	)
	keyDesc, err := keyRing.DeriveKey(keychain.KeyLocator{
		Family: keychain.KeyFamily(sparseAccount),
		Index:  0,
	})
	require.NoError(t, err)
	require.NotNil(t, keyDesc.PubKey)

	db := testWallet.Database()

	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		addrmgrNs := tx.ReadBucket(waddrmgrNamespaceKey)
		if addrmgrNs == nil {
			return fmt.Errorf("waddrmgr namespace not found")
		}

		// Ensure the sparse account number was preserved.
		customMgr, err := testWallet.Manager.FetchScopedKeyManager(
			customScope,
		)
		if err != nil {
			return err
		}

		name, err := customMgr.AccountName(addrmgrNs, sparseAccount)
		if err != nil {
			return err
		}
		expectedName := fmt.Sprintf(
			"%s/%d'", customScope.String(), sparseAccount,
		)
		if name != expectedName {
			return fmt.Errorf("expected account %d name %q, got %q",
				sparseAccount, expectedName, name)
		}

		// The old behavior would have created this account
		// contiguously.
		_, err = customMgr.AccountName(addrmgrNs, 1)
		var managerErr waddrmgr.ManagerError
		if !errors.As(err, &managerErr) ||
			managerErr.ErrorCode != waddrmgr.ErrAccountNotFound {

			return fmt.Errorf("expected account 1 to be missing, "+
				"got: %v", err)
		}

		// Ensure account 0 in the default BIP84 scope keeps its
		// "default" naming behavior.
		bip84Mgr, err := testWallet.Manager.FetchScopedKeyManager(
			waddrmgr.KeyScopeBIP0084,
		)
		if err != nil {
			return err
		}
		defaultName, err := bip84Mgr.AccountName(addrmgrNs, 0)
		if err != nil {
			return err
		}
		if defaultName != "default" {
			return fmt.Errorf("expected account 0 name default, "+
				"got %q", defaultName)
		}

		return nil
	})
	require.NoError(t, err)
}

// deriveAccountXpub derives and neuters an account-level key at
// m/purpose'/coin_type'/account'.
func deriveAccountXpub(rootKey *hdkeychain.ExtendedKey, purpose, coinType,
	account uint32) (*hdkeychain.ExtendedKey, error) {

	path := []uint32{
		purpose + hdkeychain.HardenedKeyStart,
		coinType + hdkeychain.HardenedKeyStart,
		account + hdkeychain.HardenedKeyStart,
	}

	current := rootKey
	var err error
	for _, pathPart := range path {
		current, err = current.Derive(pathPart)
		if err != nil {
			return nil, err
		}
	}

	return current.Neuter()
}
