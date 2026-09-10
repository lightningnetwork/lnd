package btcwallet

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/accountbackup"
)

// walletAccountBackup owns the independent recovery file and refresh lifetime.
// Synchronous issuing paths are authoritative; refresh also records other keys.
type walletAccountBackup struct {
	store *accountbackup.Store
	quit  chan struct{}
	wg    sync.WaitGroup
}

// accountSnapshot copies public identities and next-index bounds from all LND
// scopes. Single imported keys have no reconstructible account and are
// excluded.
func (b *BtcWallet) accountSnapshot() (*accountbackup.Snapshot, error) {
	accounts, err := b.ListAccounts("", nil)
	if err != nil {
		return nil, errors.New("read wallet accounts for backup failed")
	}
	snap := &accountbackup.Snapshot{
		Version:   1,
		Network:   b.netParams.Name,
		UpdatedAt: time.Now().UTC(),
	}
	for _, a := range accounts {
		if a.AccountNumber == waddrmgr.ImportedAddrAccount {
			continue
		}
		if a.AccountPubKey == nil || a.AccountPubKey.IsPrivate() ||
			a.AccountPubKey.ChildIndex() <
				hdkeychain.HardenedKeyStart {

			return nil, errors.New("account lacks public " +
				"recovery identity")
		}
		schema, ok := waddrmgr.ScopeAddrMap[a.KeyScope]
		if a.KeyScope == b.chainKeyScope {
			schema = lightningAddrSchema
			ok = true
		}
		if a.AddrSchema != nil {
			schema = *a.AddrSchema
			ok = true
		}
		if !ok {
			return nil, errors.New("account has unknown address " +
				"schema")
		}
		snap.Accounts = append(snap.Accounts, accountbackup.Account{
			Name:    a.AccountName,
			Purpose: a.KeyScope.Purpose,
			Coin:    a.KeyScope.Coin,
			Index: a.AccountPubKey.ChildIndex() -
				hdkeychain.HardenedKeyStart,
			ExternalType: uint8(schema.ExternalAddrType),
			InternalType: uint8(schema.InternalAddrType),
			XPub:         a.AccountPubKey.String(),
			Fingerprint:  a.MasterKeyFingerprint,
			WatchOnly:    a.IsWatchOnly,
			External:     a.ExternalKeyCount,
			Internal:     a.InternalKeyCount,
		})
	}

	return snap, nil
}

// openAccountBackup validates the surviving record before wallet RPC service.
// A restored wallet must be reconstructed in isolated maintenance first.
func (b *BtcWallet) openAccountBackup() error {
	if b.cfg.AccountBackupPath == "" {
		return nil
	}
	if b.cfg.WatchOnly {
		return errors.New("account backup currently requires a local " +
			"signing wallet")
	}
	store, err := accountbackup.Open(
		b.cfg.AccountBackupPath, b.cfg.AccountBackupCreate,
	)
	if err != nil {
		return err
	}
	snap, err := b.accountSnapshot()
	if err == nil {
		err = store.Save(snap, true, b.cfg.AccountBackupCreate)
	}
	if err != nil {
		_ = store.Close()

		return fmt.Errorf("validate account backup: %w", err)
	}
	b.accountBackup = &walletAccountBackup{
		store: store,
		quit:  make(chan struct{}),
	}

	return nil
}

// recordAccountBackup prevents a successful issuing response until its keys
// survive wallet database loss. Concurrent stale reads merge branch maxima.
func (b *BtcWallet) recordAccountBackup() error {
	if b.accountBackup == nil {
		return nil
	}
	snap, err := b.accountSnapshot()
	if err != nil {
		return err
	}
	if err = b.accountBackup.store.Save(snap, false, false); err != nil {
		return fmt.Errorf("persist account recovery before issuing "+
			"keys: %w", err)
	}

	return nil
}

// refreshAccountBackup tracks non-issuing activity. Issuing methods still
// synchronously save; a timer or readiness signal cannot close that boundary.
func (b *BtcWallet) refreshAccountBackup() {
	if b.accountBackup == nil {
		return
	}
	b.accountBackup.wg.Add(1)
	go func() {
		defer b.accountBackup.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		failed := false
		for {
			select {
			case <-b.accountBackup.quit:
				return

			case <-ticker.C:
				err := b.recordAccountBackup()
				if err != nil && !failed {
					log.Errorf("Account backup refresh "+
						"failed; repair independent "+
						"backup storage: %v", err)
				}
				if err == nil && failed {
					log.Infof("Account backup refresh " +
						"recovered")
				}
				failed = err != nil
			}
		}
	}()
}

// closeAccountBackup stops refreshes and releases the exclusive process lock.
func (b *BtcWallet) closeAccountBackup() error {
	if b.accountBackup == nil {
		return nil
	}
	close(b.accountBackup.quit)
	b.accountBackup.wg.Wait()

	return b.accountBackup.store.Close()
}

// recordNamedAccountBackup limits the new storage dependency to named-account
// issuance. Ordinary Lightning/default-account operations keep their behavior.
func (b *BtcWallet) recordNamedAccountBackup(name string) error {
	if name == "" || name == lnwallet.DefaultAccountName ||
		name == waddrmgr.ImportedAddrAccountName {

		return nil
	}

	return b.recordAccountBackup()
}
