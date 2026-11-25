//go:build kvdb_postgres

package postgres

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

// TestConnectionBudget opens many databases at once and keeps a burst of
// concurrent transactions in flight on each, which together want more
// connections than the server allows. The shared connection budget must make
// the excess transactions wait for a free connection instead of failing with
// "too many clients".
func TestConnectionBudget(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stop() })

	const (
		numDBs    = 20
		numWorker = 40
		numRounds = 5
	)

	fixtures := make([]*fixture[walletdb.DB], numDBs)
	for i := range fixtures {
		f, err := NewFixture("")
		require.NoError(t, err)

		fixtures[i] = f
		t.Cleanup(func() { _ = f.Db.Close() })
	}

	createBucket := func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket([]byte("test"))

		return err
	}

	reset := func() {}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, f := range fixtures {
		for range numWorker {
			wg.Add(1)
			go func() {
				defer wg.Done()

				for range numRounds {
					err := f.Db.Update(createBucket, reset)
					if err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
			}()
		}
	}
	wg.Wait()

	require.Empty(t, errs)
}
