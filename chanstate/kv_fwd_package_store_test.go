package chanstate_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestKVFwdPackageStore verifies that the KV channel-state store satisfies the
// forwarding-package contract used by the switch.
func TestKVFwdPackageStore(t *testing.T) {
	t.Parallel()

	backend := makeFwdPkgDB(t, "")
	source := lnwire.NewShortChanIDFromInt(42)
	height := uint64(7)
	settleFails := testSettleFails()
	fwdPkg := chanstate.NewFwdPkg(source, height, nil, settleFails)

	packager := chanstate.NewChannelPackager(source)
	err := kvdb.Update(backend, func(tx kvdb.RwTx) error {
		return packager.AddFwdPkg(tx, fwdPkg)
	}, func() {})
	require.NoError(t, err)

	var store chanstate.FwdPackageStore = chanstate.NewKVStore(backend)

	fwdPkgs, err := store.LoadChannelFwdPkgs(source)
	require.NoError(t, err)
	require.Len(t, fwdPkgs, 1)
	require.Equal(t, fwdPkg.Source, fwdPkgs[0].Source)
	require.Equal(t, fwdPkg.Height, fwdPkgs[0].Height)
	require.Len(t, fwdPkgs[0].SettleFails, len(settleFails))
	require.False(t, fwdPkgs[0].SettleFailFilter.Contains(0))

	err = store.AckSettleFailRefs(chanstate.SettleFailRef{
		Source: source,
		Height: height,
		Index:  0,
	})
	require.NoError(t, err)

	fwdPkgs, err = store.LoadChannelFwdPkgs(source)
	require.NoError(t, err)
	require.Len(t, fwdPkgs, 1)
	require.True(t, fwdPkgs[0].SettleFailFilter.Contains(0))
	require.False(t, fwdPkgs[0].SettleFailFilter.Contains(1))
}
