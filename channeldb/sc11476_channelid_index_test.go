package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestFetchChannelByIDPersistentIndex(t *testing.T) {
	fullDB, err := MakeTestDB(
		t, OptionTombstoneClosedChannels(true),
	)
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()

	serializedOutpoint := func(point wire.OutPoint) []byte {
		var buf bytes.Buffer
		require.NoError(t, graphdb.WriteOutpoint(&buf, &point))
		require.Len(t, buf.Bytes(), 36)

		return buf.Bytes()
	}

	indexValue := func(id lnwire.ChannelID) []byte {
		var value []byte
		err := kvdb.View(cdb.backend, func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket(chanIDBucket)
			require.NotNil(t, bucket)
			value = append([]byte(nil), bucket.Get(id[:])...)

			return nil
		}, func() {})
		require.NoError(t, err)

		return value
	}

	live := createTestChannel(t, cdb, openChannelOption())
	liveID := lnwire.NewChanIDFromOutPoint(live.FundingOutpoint)
	liveValue := serializedOutpoint(live.FundingOutpoint)
	require.Equal(t, liveValue, indexValue(liveID))

	got, err := cdb.FetchChannelByID(liveID)
	require.NoError(t, err)
	require.Equal(t, live.FundingOutpoint, got.FundingOutpoint)

	missingPoint := live.FundingOutpoint
	missingPoint.Index++
	missingID := lnwire.NewChanIDFromOutPoint(missingPoint)
	require.NotEqual(t, liveID, missingID)

	_, err = cdb.FetchChannelByID(missingID)
	require.ErrorIs(t, err, ErrChannelNotFound)

	closed := createTestChannel(
		t, cdb, openChannelOption(), closedChannelOption(),
	)
	closedID := lnwire.NewChanIDFromOutPoint(closed.FundingOutpoint)
	closedValue := serializedOutpoint(closed.FundingOutpoint)
	require.Equal(t, closedValue, indexValue(closedID))

	_, err = cdb.FetchChannelByID(closedID)
	require.ErrorIs(t, err, ErrChannelNotFound)

	// Preserve lookup compatibility with legacy membership-only entries.
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(chanIDBucket)
		require.NotNil(t, bucket)

		return bucket.Put(liveID[:], []byte{})
	}, func() {})
	require.NoError(t, err)

	got, err = cdb.FetchChannelByID(liveID)
	require.NoError(t, err)
	require.Equal(t, live.FundingOutpoint, got.FundingOutpoint)

	// Corrupt non-empty values must fail closed instead of scanning.
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(chanIDBucket)
		require.NotNil(t, bucket)

		return bucket.Put(liveID[:], []byte{1, 2, 3})
	}, func() {})
	require.NoError(t, err)

	_, err = cdb.FetchChannelByID(liveID)
	require.Error(t, err)

	// Restore the valid mapping for clean test teardown.
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(chanIDBucket)
		require.NotNil(t, bucket)

		return bucket.Put(liveID[:], liveValue)
	}, func() {})
	require.NoError(t, err)
}
