//go:build kvdb_postgres

package chanstate_test

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	postgresBenchPackages = 500
	postgresBenchAdds     = 50
)

var (
	postgresBenchCount int
	postgresBenchPkgs  []*chanstate.FwdPkg
)

// BenchmarkPostgresFwdPkgCount compares counting forwarding packages by fully
// loading them via LoadFwdPkgs with counting only the immediate height buckets
// via CountFwdPkgs on the PostgreSQL kvdb backend.
func BenchmarkPostgresFwdPkgCount(b *testing.B) {
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, stop())
	})

	fixture, err := postgres.NewFixture("")
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, fixture.Db.Close())
	})

	source := lnwire.NewShortChanIDFromInt(1)
	packager := chanstate.NewChannelPackager(source)
	adds := make([]chanstate.LogUpdate, postgresBenchAdds)
	chanID := lnwire.NewChanIDFromOutPoint(wire.OutPoint{})
	for i := range adds {
		adds[i] = chanstate.LogUpdate{
			LogIndex: uint64(i),
			UpdateMsg: &lnwire.UpdateAddHTLC{
				ChanID: chanID,
				ID:     uint64(i),
			},
		}
	}

	err = kvdb.Update(fixture.Db, func(tx kvdb.RwTx) error {
		for height := range postgresBenchPackages {
			pkg := chanstate.NewFwdPkg(
				source, uint64(height), adds, nil,
			)
			if err := packager.AddFwdPkg(tx, pkg); err != nil {
				return fmt.Errorf(
					"add package %d: %w", height, err,
				)
			}
		}

		return nil
	}, func() {})
	require.NoError(b, err)

	b.Run("LoadFwdPkgs", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			err := kvdb.View(fixture.Db, func(tx kvdb.RTx) error {
				var err error
				postgresBenchPkgs, err = packager.LoadFwdPkgs(
					tx,
				)

				return err
			}, func() {})
			require.NoError(b, err)
			require.Len(b, postgresBenchPkgs, postgresBenchPackages)
		}
	})

	b.Run("ForAllCount", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			err := kvdb.View(fixture.Db, func(tx kvdb.RTx) error {
				var err error
				postgresBenchCount, err = packager.CountFwdPkgs(
					tx,
				)

				return err
			}, func() {})
			require.NoError(b, err)
			require.Equal(
				b, postgresBenchPackages, postgresBenchCount,
			)
		}
	})
}
