package chanstate

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func makeTestBackend(t *testing.T) (kvdb.Backend, func()) {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "cdb")
	require.NoError(t, err)

	return backend, cleanup
}

// TestFinalHtlcs tests final htlc storage and retrieval.
func TestFinalHtlcs(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithStoreFinalHtlcResolutions(true))

	chanID := lnwire.ShortChannelID{
		BlockHeight: 1,
		TxIndex:     2,
		TxPosition:  3,
	}

	// Test unknown htlc lookup.
	const unknownHtlcID = 999

	_, err := store.LookupFinalHtlc(chanID, unknownHtlcID)
	require.ErrorIs(t, err, ErrHtlcUnknown)

	// Test offchain final htlcs.
	const offchainHtlcID = 1

	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		bucket, err := FetchFinalHtlcsBucketRw(
			tx, chanID,
		)
		require.NoError(t, err)

		return PutFinalHtlc(
			bucket, offchainHtlcID, FinalHtlcInfo{
				Settled:  true,
				Offchain: true,
			},
		)
	}, func() {})
	require.NoError(t, err)

	info, err := store.LookupFinalHtlc(chanID, offchainHtlcID)
	require.NoError(t, err)
	require.True(t, info.Settled)
	require.True(t, info.Offchain)

	// Test onchain final htlcs.
	const onchainHtlcID = 2

	err = store.PutOnchainFinalHtlcOutcome(chanID, onchainHtlcID, true)
	require.NoError(t, err)

	info, err = store.LookupFinalHtlc(chanID, onchainHtlcID)
	require.NoError(t, err)
	require.True(t, info.Settled)
	require.False(t, info.Offchain)

	// Test unknown htlc lookup for existing channel.
	_, err = store.LookupFinalHtlc(chanID, unknownHtlcID)
	require.ErrorIs(t, err, ErrHtlcUnknown)
}
