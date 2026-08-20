package channeldb_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// benchFwdPkgScenario describes the shape of the synthetic fwd pkg DB used by
// the load benchmarks: numChannels independent channels, each carrying
// pkgsPerChannel forwarding packages with a mix of adds and settle/fails.
type benchFwdPkgScenario struct {
	numChannels    int
	pkgsPerChannel int
}

// populateFwdPkgs writes a synthetic fwd pkg database for the provided
// scenario. All channels are populated within a single write transaction so
// that benchmark setup is dominated by the work under test rather than by the
// fsync cost of one write tx per channel.
func populateFwdPkgs(b *testing.B, db kvdb.Backend,
	scenario benchFwdPkgScenario) []lnwire.ShortChannelID {

	b.Helper()

	sources := make([]lnwire.ShortChannelID, 0, scenario.numChannels)
	for i := 0; i < scenario.numChannels; i++ {
		// Skip 0 to avoid collisions with hop.Source-style sentinels.
		shortChanID := lnwire.NewShortChanIDFromInt(uint64(i + 1))
		sources = append(sources, shortChanID)
	}

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		for _, shortChanID := range sources {
			packager := channeldb.NewChannelPackager(shortChanID)
			for h := 0; h < scenario.pkgsPerChannel; h++ {
				fwdPkg := channeldb.NewFwdPkg(
					shortChanID, uint64(h),
					testAdds(), testSettleFails(),
				)
				if err := packager.AddFwdPkg(
					tx, fwdPkg,
				); err != nil {
					return err
				}
			}
		}

		return nil
	}, func() {})
	require.NoError(b, err)

	return sources
}

// benchScenarios is the matrix exercised by the load benchmarks. The 1k+ case
// matches the motivating production scenario.
var benchScenarios = []benchFwdPkgScenario{
	{numChannels: 100, pkgsPerChannel: 1},
	{numChannels: 500, pkgsPerChannel: 1},
	{numChannels: 1000, pkgsPerChannel: 1},
	{numChannels: 2000, pkgsPerChannel: 1},
	{numChannels: 1000, pkgsPerChannel: 4},
}

// BenchmarkLoadChannelFwdPkgsPerTx measures the prior strategy of opening one
// read transaction per channel to load that channel's forwarding packages.
func BenchmarkLoadChannelFwdPkgsPerTx(b *testing.B) {
	for _, scenario := range benchScenarios {
		scenario := scenario
		name := benchName(scenario)
		b.Run(name, func(b *testing.B) {
			db := makeFwdPkgBenchDB(b)
			sources := populateFwdPkgs(b, db, scenario)

			pkgr := channeldb.NewSwitchPackager()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				totalPkgs := 0
				for _, source := range sources {
					var fwdPkgs []*channeldb.FwdPkg
					err := kvdb.View(db,
						func(tx kvdb.RTx) error {
							var err error
							fwdPkgs, err = pkgr.LoadChannelFwdPkgs( //nolint:lll
								tx, source,
							)
							return err
						}, func() {
							fwdPkgs = nil
						},
					)
					if err != nil {
						b.Fatal(err)
					}
					totalPkgs += len(fwdPkgs)
				}

				if totalPkgs == 0 {
					b.Fatal("expected fwd pkgs to be loaded")
				}
			}
		})
	}
}

// BenchmarkLoadChannelFwdPkgsSet measures the batched query which loads
// forwarding packages for the entire set of channels in a single read
// transaction.
func BenchmarkLoadChannelFwdPkgsSet(b *testing.B) {
	for _, scenario := range benchScenarios {
		scenario := scenario
		name := benchName(scenario)
		b.Run(name, func(b *testing.B) {
			db := makeFwdPkgBenchDB(b)
			sources := populateFwdPkgs(b, db, scenario)

			pkgr := channeldb.NewSwitchPackager()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var fwdPkgs []*channeldb.FwdPkg
				err := kvdb.View(db, func(tx kvdb.RTx) error {
					var err error
					fwdPkgs, err = pkgr.LoadChannelFwdPkgsSet( //nolint:lll
						tx, sources,
					)
					return err
				}, func() {
					fwdPkgs = nil
				})
				if err != nil {
					b.Fatal(err)
				}

				if len(fwdPkgs) == 0 {
					b.Fatal("expected fwd pkgs to be loaded")
				}
			}
		})
	}
}

// benchName produces a deterministic, parseable sub-benchmark name. This shape
// is important so that benchstat can pair the per-tx and batched variants when
// computing deltas.
func benchName(s benchFwdPkgScenario) string {
	return "channels=" + itoa(s.numChannels) +
		"/pkgs=" + itoa(s.pkgsPerChannel)
}

// itoa is a tiny strconv.Itoa stand-in to avoid pulling strconv into the test
// file solely for benchmark naming.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

// makeFwdPkgBenchDB constructs a fresh on-disk fwd pkg database for the
// benchmark to operate on. We use the standard bolt backend here since this is
// the same backend used by lnd in practice for fwd pkg storage.
func makeFwdPkgBenchDB(b *testing.B) kvdb.Backend {
	b.Helper()

	path := b.TempDir() + "/fwdpkg.db"
	bdb, err := kvdb.Create(
		kvdb.BoltBackendName, path, true, kvdb.DefaultDBTimeout, false,
	)
	require.NoError(b, err, "unable to open boltdb")

	b.Cleanup(func() {
		_ = bdb.Close()
	})

	return bdb
}
