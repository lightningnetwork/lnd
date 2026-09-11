// Package accountbackup durably records the public metadata needed to rebuild
// named wallet accounts after loss of the wallet database.
package accountbackup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
)

// Account identifies a derivation scope and its two next unused key indices.
// Public keys are sensitive wallet metadata, but this record holds no secrets.
type Account struct {
	Name         string `json:"name"`
	Purpose      uint32 `json:"purpose"`
	Coin         uint32 `json:"coin"`
	Index        uint32 `json:"account_index"`
	ExternalType uint8  `json:"external_address_type"`
	InternalType uint8  `json:"internal_address_type"`
	XPub         string `json:"extended_public_key"`
	Fingerprint  uint32 `json:"master_key_fingerprint"`
	WatchOnly    bool   `json:"watch_only"`
	External     uint32 `json:"external_key_count"`
	Internal     uint32 `json:"internal_key_count"`
}

// Snapshot is a versioned public recovery record. Counts describe issued key
// bounds, not discovered balances. Network prevents cross-chain substitution.
type Snapshot struct {
	Version   uint32    `json:"version"`
	Network   string    `json:"network"`
	UpdatedAt time.Time `json:"updated_at"`
	Accounts  []Account `json:"accounts"`
}

// Store owns the stable lock file for its lifetime. A second wallet process
// cannot share this backup. Save serializes concurrent in-process writers.
type Store struct {
	mu   sync.Mutex
	path string
	lock kvdb.Backend
}

// Open locks path's sibling lock database. Creation is an explicit one-time
// operation; normal startup refuses a missing or corrupt surviving record.
func Open(path string, create bool) (*Store, error) {
	lock, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:     filepath.Dir(path),
		DBFileName: filepath.Base(path) + ".lock",
		DBTimeout:  time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("lock account backup: %w", err)
	}
	s := &Store{path: path, lock: lock}
	_, err = os.Stat(path)
	if create {
		if !errors.Is(err, os.ErrNotExist) {
			_ = lock.Close()

			return nil, errors.New("account backup creation " +
				"requires a new path")
		}
	} else if err != nil {
		_ = lock.Close()

		return nil, errors.New("account backup missing or " +
			"inaccessible; restore surviving evidence")
	}

	return s, nil
}

// Close releases the exclusive writer lock after wallet use stops.
func (s *Store) Close() error { return s.lock.Close() }

// Decode validates a snapshot without including wallet metadata in errors.
// Explicit branch fields distinguish a valid zero from a truncated export.
func Decode(data []byte) (*Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, errors.New("invalid account backup JSON")
	}
	if snap.Version != 1 || snap.Network == "" || len(snap.Accounts) == 0 {
		return nil, errors.New("invalid account backup header")
	}
	var raw struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("invalid account records")
	}
	seen := make(map[string]bool)
	for i, a := range snap.Accounts {
		if a.Name == "" || a.XPub == "" || seen[a.key()] {
			return nil, errors.New("invalid or duplicate account " +
				"identity")
		}
		for _, field := range []string{
			"external_key_count",
			"internal_key_count",
		} {
			v, ok := raw.Accounts[i][field]
			if !ok || bytes.Equal(v, []byte("null")) {
				return nil, errors.New("account backup " +
					"missing branch count")
			}
		}
		seen[a.key()] = true
	}

	return &snap, nil
}

// key uses the wallet account name and scope. Imported xpubs from different
// seeds may have the same child index, so the derivation index is not unique.
func (a Account) key() string {
	return fmt.Sprintf("%d/%d/%s", a.Purpose, a.Coin, a.Name)
}

// identity removes the only mutable account fields before comparison.
func (a Account) identity() Account {
	a.External = 0
	a.Internal = 0
	return a
}

// Merge preserves identities and branch maxima. Startup additionally requires
// the live wallet to contain every historical account and both recorded bounds.
func Merge(old, live *Snapshot, startup bool) (*Snapshot, error) {
	if old.Version != live.Version || old.Network != live.Network {
		return nil, errors.New("account backup wallet/network mismatch")
	}
	result := *live
	result.Accounts = append([]Account(nil), live.Accounts...)
	positions := make(map[string]int)
	for i, a := range result.Accounts {
		positions[a.key()] = i
	}
	for _, previous := range old.Accounts {
		i, ok := positions[previous.key()]
		if !ok {
			if startup {
				return nil, errors.New("reconstruct missing " +
					"recorded account before startup")
			}
			result.Accounts = append(result.Accounts, previous)

			continue
		}
		current := &result.Accounts[i]
		if current.identity() != previous.identity() {
			return nil, errors.New("account backup identity " +
				"mismatch")
		}
		if startup && previous.Purpose != 1017 &&
			previous.Name != "default" &&
			(current.External < previous.External ||
				current.Internal < previous.Internal) {

			return nil, errors.New("reconstruct both " +
				"named-account branches before startup")
		}
		current.External = max(current.External, previous.External)
		current.Internal = max(current.Internal, previous.Internal)
	}

	return &result, nil
}

// Save acknowledges only after file and directory synchronization. Failed
// writes never grant permission to expose a newly allocated address or PSBT.
func (s *Store) Save(live *Snapshot, startup, create bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(live)
	if err != nil {
		return err
	}
	live, err = Decode(encoded)
	if err != nil {
		return err
	}
	if !create {
		data, err := os.ReadFile(s.path)
		if err != nil {
			return errors.New("read surviving account backup " +
				"failed")
		}
		old, err := Decode(data)
		if err != nil {
			return err
		}
		live, err = Merge(old, live, startup)
		if err != nil {
			return err
		}
	} else if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("account backup already exists or is " +
			"inaccessible")
	}
	encoded, err = json.MarshalIndent(live, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".account-backup-*")
	if err != nil {
		return errors.New("create account backup temporary file failed")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()

		return errors.New("write account backup failed")
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()

		return errors.New("sync account backup failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("close account backup failed")
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return errors.New("replace account backup failed")
	}
	parent, err := os.Open(dir)
	if err != nil {
		return errors.New("open account backup directory failed")
	}
	defer parent.Close()
	if err = parent.Sync(); err != nil {
		return errors.New("sync account backup directory failed")
	}

	return nil
}
