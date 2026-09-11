package accountbackup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixture describes a named account with real nonzero internal change usage.
func fixture() *Snapshot {
	return &Snapshot{
		Version:   1,
		Network:   "regtest",
		UpdatedAt: time.Now().UTC(),
		Accounts: []Account{
			{
				Name:     "treasury",
				Purpose:  86,
				Coin:     0,
				Index:    2,
				XPub:     "public-test-identity",
				External: 3,
				Internal: 7,
			},
		},
	}
}

// TestMonotonicStore exercises concurrent stale writers, restart, exclusive
// process ownership, and a failed identity update without destroying evidence.
func TestMonotonicStore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := Open(path, true)
	require.NoError(t, err)
	require.NoError(t, s.Save(fixture(), true, true))
	_, err = Open(path, false)
	require.Error(t, err)
	var wg sync.WaitGroup
	for i := uint32(0); i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := fixture()
			snap.Accounts[0].External = i
			snap.Accounts[0].Internal = 20 - i
			require.NoError(t, s.Save(snap, false, false))
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	saved, err := Decode(data)
	require.NoError(t, err)
	require.EqualValues(t, 19, saved.Accounts[0].External)
	require.EqualValues(t, 20, saved.Accounts[0].Internal)
	changed := fixture()
	changed.Accounts[0].XPub = "different-wallet"
	require.ErrorContains(
		t, s.Save(changed, false, false),
		"identity mismatch",
	)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, after)
	require.NoError(t, s.Close())
	s, err = Open(path, false)
	require.NoError(t, err)
	defer s.Close()
	require.ErrorContains(
		t,
		s.Save(
			fixture(), true, false,
		),
		"both named-account branches",
	)
	require.NoError(t, s.Save(saved, true, false))
}

// TestRecoveryNegativeControls proves stale or missing internal counts cannot
// pass the reconstruction invariant even when external counts match exactly.
func TestRecoveryNegativeControls(t *testing.T) {
	t.Parallel()
	old := fixture()
	live := fixture()
	live.Accounts[0].Internal--
	_, err := Merge(old, live, true)
	require.ErrorContains(t, err, "both named-account branches")
	merged, err := Merge(old, live, false)
	require.NoError(t, err)
	require.EqualValues(t, 7, merged.Accounts[0].Internal)
	data, err := json.Marshal(old)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	accounts, ok := raw["accounts"].([]any)
	require.True(t, ok)
	account, ok := accounts[0].(map[string]any)
	require.True(t, ok)
	delete(account, "internal_key_count")
	data, err = json.Marshal(raw)
	require.NoError(t, err)
	_, err = Decode(data)
	require.ErrorContains(t, err, "missing branch count")
	live = fixture()
	live.Accounts = nil
	_, err = Merge(old, live, true)
	require.ErrorContains(t, err, "missing recorded account")
}

// TestBackupLossFailsClosed requires surviving evidence after enrollment;
// disappearance never silently enrolls the wallet again.
func TestBackupLossFailsClosed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "accounts.json")
	_, err := Open(path, false)
	require.Error(t, err)
	s, err := Open(path, true)
	require.NoError(t, err)
	require.NoError(t, s.Save(fixture(), true, true))
	require.NoError(t, os.Remove(path))
	require.Error(t, s.Save(fixture(), false, false))
	require.NoError(t, s.Close())
	_, err = Open(path, false)
	require.Error(t, err)
}

// TestImportedIdentity preserves independent xpubs with equal child indices,
// while rejecting a recreated name bound to a different derivation index.
func TestImportedIdentity(t *testing.T) {
	t.Parallel()
	snap := fixture()
	other := snap.Accounts[0]
	other.Name = "external"
	other.XPub = "other-public-key"
	other.WatchOnly = true
	snap.Accounts = append(snap.Accounts, other)
	data, err := json.Marshal(snap)
	require.NoError(t, err)
	_, err = Decode(data)
	require.NoError(t, err)
	changed := fixture()
	changed.Accounts[0].Index++
	_, err = Merge(fixture(), changed, false)
	require.ErrorContains(t, err, "identity mismatch")
}
