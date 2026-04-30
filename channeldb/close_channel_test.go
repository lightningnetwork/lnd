package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// writeTestRevlogEntries writes n entries directly into the
// revocationLogBucket of the given channel. The helper navigates the raw KV
// tree so the test does not depend on the higher-level commit-chain
// machinery.
func writeTestRevlogEntries(t *testing.T, ch *OpenChannel, n int) {
	t.Helper()

	err := kvdb.Update(ch.Db.backend, func(tx kvdb.RwTx) error {
		openChanBkt := tx.ReadWriteBucket(openChannelBucket)
		require.NotNil(t, openChanBkt, "openChannelBucket missing")

		nodePub := ch.IdentityPub.SerializeCompressed()
		nodeBkt := openChanBkt.NestedReadWriteBucket(nodePub)
		require.NotNil(t, nodeBkt, "node bucket missing")

		chainBkt := nodeBkt.NestedReadWriteBucket(ch.ChainHash[:])
		require.NotNil(t, chainBkt, "chain bucket missing")

		var chanKeyBuf bytes.Buffer
		err := graphdb.WriteOutpoint(&chanKeyBuf, &ch.FundingOutpoint)
		require.NoError(t, err)

		chanBkt := chainBkt.NestedReadWriteBucket(chanKeyBuf.Bytes())
		require.NotNil(t, chanBkt, "channel bucket missing")

		logBkt, err := chanBkt.CreateBucketIfNotExists(
			revocationLogBucket,
		)
		require.NoError(t, err)

		for i := range n {
			commit := testChannelCommit
			commit.CommitHeight = uint64(i)

			err := putRevocationLog(logBkt, &commit, 0, 1, false)
			require.NoError(t, err)
		}

		return nil
	}, func() {})
	require.NoError(t, err)
}

// writeTestForwardingPackages writes n empty forwarding packages for the
// given channel using distinct remote commitment heights.
func writeTestForwardingPackages(t *testing.T, ch *OpenChannel, n int) {
	t.Helper()

	packager := NewChannelPackager(ch.ShortChanID())
	err := kvdb.Update(ch.Db.backend, func(tx kvdb.RwTx) error {
		for i := range n {
			pkg := NewFwdPkg(
				ch.ShortChanID(), uint64(i), nil, nil,
			)
			if err := packager.AddFwdPkg(tx, pkg); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)
}

// countRevlogEntries returns the number of entries in the revocationLogBucket
// for the given channel, or -1 if the channel bucket no longer exists in
// openChannelBucket.
func countRevlogEntries(t *testing.T, ch *OpenChannel) int {
	t.Helper()

	count := -1
	err := kvdb.View(ch.Db.backend, func(tx kvdb.RTx) error {
		openChanBkt := tx.ReadBucket(openChannelBucket)
		if openChanBkt == nil {
			return nil
		}

		nodePub := ch.IdentityPub.SerializeCompressed()
		nodeBkt := openChanBkt.NestedReadBucket(nodePub)
		if nodeBkt == nil {
			return nil
		}

		chainBkt := nodeBkt.NestedReadBucket(ch.ChainHash[:])
		if chainBkt == nil {
			return nil
		}

		var chanKeyBuf bytes.Buffer
		if err := graphdb.WriteOutpoint(
			&chanKeyBuf, &ch.FundingOutpoint,
		); err != nil {
			return err
		}

		chanBkt := chainBkt.NestedReadBucket(chanKeyBuf.Bytes())
		if chanBkt == nil {
			return nil
		}

		logBkt := chanBkt.NestedReadBucket(revocationLogBucket)
		if logBkt == nil {
			count = 0
			return nil
		}

		c := 0
		if err := logBkt.ForEach(func(_, _ []byte) error {
			c++
			return nil
		}); err != nil {
			return err
		}

		count = c

		return nil
	}, func() {})
	require.NoError(t, err)

	return count
}

// readOutpointStatus decodes the indexStatus TLV byte stored under
// outpointBucket for the given outpoint. Used to verify the index flip
// performed by the close path.
func readOutpointStatus(t *testing.T, cdb *ChannelStateDB,
	op wire.OutPoint) indexStatus {

	t.Helper()

	var chanKeyBuf bytes.Buffer
	require.NoError(t, graphdb.WriteOutpoint(&chanKeyBuf, &op))

	var status uint8
	err := kvdb.View(cdb.backend, func(tx kvdb.RTx) error {
		bkt := tx.ReadBucket(outpointBucket)
		require.NotNil(t, bkt, "outpointBucket missing")

		raw := bkt.Get(chanKeyBuf.Bytes())
		require.NotNil(t, raw, "outpoint entry missing")

		statusRecord := tlv.MakePrimitiveRecord(
			indexStatusType, &status,
		)
		stream, err := tlv.NewStream(statusRecord)
		if err != nil {
			return err
		}

		return stream.Decode(bytes.NewReader(raw))
	}, func() {})
	require.NoError(t, err)

	return indexStatus(status)
}

// closeChannelForTest invokes CloseChannel on a freshly created OpenChannel
// using a minimal close summary derived from the channel state itself.
func closeChannelForTest(t *testing.T, cdb *ChannelStateDB, ch *OpenChannel) {
	t.Helper()

	summary := &ChannelCloseSummary{
		ChanPoint:   ch.FundingOutpoint,
		RemotePub:   ch.IdentityPub,
		ChainHash:   ch.ChainHash,
		ShortChanID: ch.ShortChannelID,
		CloseType:   CooperativeClose,
	}
	require.NoError(t, cdb.CloseChannel(ch, summary))
}

// TestCloseChannelTombstoneWritePath verifies the on-disk artefacts the
// tombstone close path produces in a single write transaction: the outpoint
// index flips from open to closed, the historical-channel record and close
// summary are written, and the bulk per-channel state (revocation log,
// forwarding packages) is left intact — that retention is the entire reason
// for the tombstone path on these backends.
func TestCloseChannelTombstoneWritePath(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t, OptionTombstoneClosedChannels(true))
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()
	require.True(t, cdb.tombstoneClosedChannels)

	ch := createTestChannel(t, cdb, openChannelOption())

	const numRevlogEntries = 5
	const numFwdPkgs = 3
	writeTestRevlogEntries(t, ch, numRevlogEntries)
	writeTestForwardingPackages(t, ch, numFwdPkgs)

	closeChannelForTest(t, cdb, ch)

	// Outpoint index flipped from open to closed — the authoritative
	// closed-channel marker on tombstone backends.
	require.Equal(t, outpointClosed, readOutpointStatus(
		t, cdb, ch.FundingOutpoint,
	))

	// Historical-channel record exists for this chanKey.
	histChan, err := cdb.FetchHistoricalChannel(&ch.FundingOutpoint)
	require.NoError(t, err)
	require.Equal(t, ch.FundingOutpoint, histChan.FundingOutpoint)

	// Close summary readable via FetchClosedChannel.
	closeSummary, err := cdb.FetchClosedChannel(&ch.FundingOutpoint)
	require.NoError(t, err)
	require.Equal(t, ch.FundingOutpoint, closeSummary.ChanPoint)

	// Bulk state preserved on disk — tombstoning's whole point.
	require.Equal(t, numRevlogEntries, countRevlogEntries(t, ch))

	packager := NewChannelPackager(ch.ShortChanID())
	var fwdPkgs []*FwdPkg
	require.NoError(t, kvdb.View(cdb.backend, func(tx kvdb.RTx) error {
		fwdPkgs, err = packager.LoadFwdPkgs(tx)
		return err
	}, func() {}))
	require.Len(t, fwdPkgs, numFwdPkgs)
}

// TestCloseChannelTombstoneRedundantClose verifies that a second CloseChannel
// call against an already-closed channel is rejected with ErrChannelNotFound
// rather than silently re-archiving or re-flipping the outpoint. The guard
// lives in locateOpenChannel.
func TestCloseChannelTombstoneRedundantClose(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t, OptionTombstoneClosedChannels(true))
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()
	ch := createTestChannel(t, cdb, openChannelOption())

	closeChannelForTest(t, cdb, ch)

	summary := &ChannelCloseSummary{
		ChanPoint:   ch.FundingOutpoint,
		RemotePub:   ch.IdentityPub,
		ChainHash:   ch.ChainHash,
		ShortChanID: ch.ShortChannelID,
		CloseType:   CooperativeClose,
	}
	require.ErrorIs(t, cdb.CloseChannel(ch, summary), ErrChannelNotFound)
}

// TestCloseChannelSync exercises the synchronous one-shot close path used by
// backends that do not opt in to tombstones (bbolt, etcd). It locks in the
// invariant that after CloseChannel returns the channel bucket and its
// revocation-log entries are already gone, and that the close summary,
// historical record, and outpoint flip are all in place.
func TestCloseChannelSync(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()
	require.False(t, cdb.tombstoneClosedChannels)

	ch := createTestChannel(t, cdb, openChannelOption())

	const numRevlogEntries = 4
	writeTestRevlogEntries(t, ch, numRevlogEntries)
	writeTestForwardingPackages(t, ch, 3)

	closeChannelForTest(t, cdb, ch)

	// The synchronous path wipes the chanBucket inline, so
	// countRevlogEntries must report -1 (bucket is gone, not just empty).
	require.Equal(t, -1, countRevlogEntries(t, ch),
		"channel bucket must be deleted after sync close")

	// Forwarding packages are wiped inline.
	var fwdPkgs []*FwdPkg
	packager := NewChannelPackager(ch.ShortChanID())
	require.NoError(t, kvdb.View(cdb.backend, func(tx kvdb.RTx) error {
		fwdPkgs, err = packager.LoadFwdPkgs(tx)
		return err
	}, func() {}))
	require.Empty(t, fwdPkgs)

	// The outpoint index reflects the close.
	require.Equal(t, outpointClosed, readOutpointStatus(
		t, cdb, ch.FundingOutpoint,
	))
}
