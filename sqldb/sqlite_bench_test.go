//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// BenchmarkSqliteMaxConns benchmarks sequential reads against a SQLite
// database with varying MaxConnections settings.
//
// Run with:
//
//	go test -bench=BenchmarkSqliteMaxConns -benchmem -run=^$ ./sqldb/
func BenchmarkSqliteMaxConns(b *testing.B) {
	const numInvoices = 500

	// connCounts contains the MaxConnections values we want to compare.
	// 0 means "use the library default" (currently 2 for SQLite).
	connCounts := []int{1, 2, 4, 8, 16, 0}

	// Build a fresh SQLite database that will be shared across all
	// sub-benchmarks. We insert a fixed set of invoices once and then
	// execute read-only queries from multiple goroutines.
	dbFileName := filepath.Join(b.TempDir(), "bench.db")

	// Open the store once with migrations so the schema is in place.
	setupStore, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbFileName)
	require.NoError(b, err)

	require.NoError(b, setupStore.ApplyAllMigrations(
		context.Background(), GetMigrations(),
	))

	ctx := context.Background()

	// Insert test invoices. We use a predictable hash per invoice so we
	// can look them up deterministically during the benchmark.
	hashes := make([][]byte, numInvoices)
	for i := range numInvoices {
		hash := make([]byte, 32)
		hash[0] = byte(i)
		hash[1] = byte(i >> 8)
		hashes[i] = hash

		_, err := setupStore.InsertInvoice(
			ctx, sqlc.InsertInvoiceParams{
				Hash:               hash,
				PaymentAddr:        hash,
				PaymentRequestHash: hash,
				Expiry:             3600,
				CreatedAt:          time.Now(),
			},
		)
		require.NoError(b, err)
	}

	require.NoError(b, setupStore.DB.Close())

	for _, maxConns := range connCounts {
		name := fmt.Sprintf("MaxConns=%d", maxConns)
		if maxConns == 0 {
			name = fmt.Sprintf("MaxConns=default(%d)",
				DefaultSqliteMaxConns)
		}

		b.Run(name, func(b *testing.B) {
			store, err := NewSqliteStore(
				&SqliteConfig{
					SkipMigrations: true,
					MaxConnections: maxConns,
				}, dbFileName,
			)
			require.NoError(b, err)

			b.Cleanup(func() {
				require.NoError(b, store.DB.Close())
			})

			var i int
			for b.Loop() {
				hash := hashes[i%numInvoices]
				i++

				_, err := store.GetInvoice(
					ctx,
					sqlc.GetInvoiceParams{
						Hash: hash,
					},
				)
				if err != nil {
					require.ErrorIs(b, err, sql.ErrNoRows)
				}
			}
		})
	}
}

// BenchmarkSqliteMaxConnsConcurrentReads measures aggregate read throughput
// for a fixed level of goroutine concurrency to complement the sequential
// benchmark above. Each iteration launches a fixed number of goroutines that
// all issue reads simultaneously, directly stressing the connection pool.
func BenchmarkSqliteMaxConnsConcurrentReads(b *testing.B) {
	const (
		numInvoices = 500
		goroutines  = 16
	)

	connCounts := []int{1, 2, 4, 8, 16, 0}

	dbFileName := filepath.Join(b.TempDir(), "bench_conc.db")

	setupStore, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbFileName)
	require.NoError(b, err)

	require.NoError(b, setupStore.ApplyAllMigrations(
		context.Background(), GetMigrations(),
	))

	ctx := context.Background()

	hashes := make([][]byte, numInvoices)
	for i := range numInvoices {
		hash := make([]byte, 32)
		hash[0] = byte(i)
		hash[1] = byte(i >> 8)
		hashes[i] = hash

		_, err := setupStore.InsertInvoice(
			ctx, sqlc.InsertInvoiceParams{
				Hash:               hash,
				PaymentAddr:        hash,
				PaymentRequestHash: hash,
				Expiry:             3600,
				CreatedAt:          time.Now(),
			},
		)
		require.NoError(b, err)
	}

	require.NoError(b, setupStore.DB.Close())

	for _, maxConns := range connCounts {
		name := fmt.Sprintf("MaxConns=%d", maxConns)
		if maxConns == 0 {
			name = fmt.Sprintf("MaxConns=default(%d)",
				DefaultSqliteMaxConns)
		}

		b.Run(name, func(b *testing.B) {
			store, err := NewSqliteStore(
				&SqliteConfig{
					SkipMigrations: true,
					MaxConnections: maxConns,
				}, dbFileName,
			)
			require.NoError(b, err)

			b.Cleanup(func() {
				require.NoError(b, store.DB.Close())
			})

			for b.Loop() {
				var wg sync.WaitGroup
				wg.Add(goroutines)

				for g := range goroutines {
					go func() {
						defer wg.Done()

						hash := hashes[g%numInvoices]
						_, err := store.GetInvoice(
							ctx,
							sqlc.GetInvoiceParams{
								Hash: hash,
							},
						)
						if err != nil &&
							err != sql.ErrNoRows {

							b.Errorf("GetInvoice:"+
								" %v", err)
						}
					}()
				}

				wg.Wait()
			}
		})
	}
}
