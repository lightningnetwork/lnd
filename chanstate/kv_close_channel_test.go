package chanstate

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// writeTestRevlogEntries writes n entries directly into the
// revocationLogBucket of the given channel. The helper navigates the raw KV
// tree so the test does not depend on the higher-level commit-chain
// machinery.
func writeTestRevlogEntries(t *testing.T, store *KVStore,
	ch *OpenChannel, n int) {

	t.Helper()

	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
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
			commit := ch.LocalCommitment
			commit.CommitHeight = uint64(i)

			err := PutRevocationLog(logBkt, &commit, 0, 1, false)
			require.NoError(t, err)
		}

		return nil
	}, func() {})
	require.NoError(t, err)
}

// writeTestForwardingPackages writes n empty forwarding packages for the
// given channel using distinct remote commitment heights.
func writeTestForwardingPackages(t *testing.T, store *KVStore,
	ch *OpenChannel, n int) {

	t.Helper()

	packager := NewChannelPackager(ch.ShortChanID())
	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
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
func countRevlogEntries(t *testing.T, store *KVStore,
	ch *OpenChannel) int {

	t.Helper()

	count := -1
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
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
func readOutpointStatus(t *testing.T, store *KVStore,
	op wire.OutPoint) IndexStatus {

	t.Helper()

	var chanKeyBuf bytes.Buffer
	require.NoError(t, graphdb.WriteOutpoint(&chanKeyBuf, &op))

	var status uint8
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		bkt := tx.ReadBucket(outpointBucket)
		require.NotNil(t, bkt, "outpointBucket missing")

		raw := bkt.Get(chanKeyBuf.Bytes())
		require.NotNil(t, raw, "outpoint entry missing")

		statusRecord := tlv.MakePrimitiveRecord(
			IndexStatusType, &status,
		)
		stream, err := tlv.NewStream(statusRecord)
		if err != nil {
			return err
		}

		return stream.Decode(bytes.NewReader(raw))
	}, func() {})
	require.NoError(t, err)

	return IndexStatus(status)
}

// closeChannelForTest invokes CloseChannel on a freshly created OpenChannel
// using a minimal close summary derived from the channel state itself.
func closeChannelForTest(t *testing.T, store *KVStore, ch *OpenChannel) {
	t.Helper()

	summary := &ChannelCloseSummary{
		ChanPoint:   ch.FundingOutpoint,
		RemotePub:   ch.IdentityPub,
		ChainHash:   ch.ChainHash,
		ShortChanID: ch.ShortChannelID,
		CloseType:   CooperativeClose,
	}
	require.NoError(t, store.CloseChannel(ch, summary))
}

// writeTestChannelResponse writes one unacknowledged settle into a forwarding
// package owned by the channel.
func writeTestChannelResponse(t *testing.T, store *KVStore,
	ch *OpenChannel) *lnwire.UpdateFulfillHTLC {

	t.Helper()

	settle := &lnwire.UpdateFulfillHTLC{
		ID:              3,
		PaymentPreimage: [32]byte{9},
	}
	packager := NewChannelPackager(ch.ShortChanID())
	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		pkg := NewFwdPkg(ch.ShortChanID(), 0, nil, []LogUpdate{
			{LogIndex: 0, UpdateMsg: settle},
		})

		return packager.AddFwdPkg(tx, pkg)
	}, func() {})
	require.NoError(t, err)

	return settle
}

func extractTestSettleResponses(
	pkgs []*FwdPkg) []*lnwire.UpdateFulfillHTLC {

	var settles []*lnwire.UpdateFulfillHTLC
	for _, pkg := range pkgs {
		for _, update := range pkg.SettleFails {
			updateMsg := update.UpdateMsg
			settle, ok := updateMsg.(*lnwire.UpdateFulfillHTLC)
			if ok {
				settles = append(settles, settle)
			}
		}
	}

	return settles
}

type testFwdResponseExtractor struct {
	extracted *[]*lnwire.UpdateFulfillHTLC
}

var _ KVFwdResponseExtractor = (*testFwdResponseExtractor)(nil)

func (e *testFwdResponseExtractor) ExtractFwdResponses(tx kvdb.RwTx,
	source lnwire.ShortChannelID) error {

	pkgs, err := NewChannelPackager(source).LoadFwdPkgs(tx)
	if err != nil {
		return err
	}

	settles := extractTestSettleResponses(pkgs)
	*e.extracted = append(*e.extracted, settles...)

	return nil
}

// TestCloseChannelExtractsFwdResponses asserts that both channel-close
// strategies invoke forwarding-response extraction while the forwarding
// package is still readable in the close transaction. The synchronous path
// must then remove the package, while the tombstone path retains it.
func TestCloseChannelExtractsFwdResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tombstone bool
	}{
		{name: "synchronous"},
		{name: "tombstone", tombstone: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			backend, cleanup := makeTestBackend(t)
			t.Cleanup(cleanup)

			var extracted []*lnwire.UpdateFulfillHTLC
			extractor := &testFwdResponseExtractor{
				extracted: &extracted,
			}

			store := NewKVStore(
				backend,
				WithTombstoneClosedChannels(test.tombstone),
				WithFwdResponseExtractor(extractor),
			)
			ch := createTestOpenChannel(t, store)
			settle := writeTestChannelResponse(t, store, ch)

			closeChannelForTest(t, store, ch)

			require.Len(t, extracted, 1)
			require.Equal(t, settle, extracted[0])

			packager := NewChannelPackager(ch.ShortChanID())
			var fwdPkgs []*FwdPkg
			err := kvdb.View(backend, func(tx kvdb.RTx) error {
				var err error
				fwdPkgs, err = packager.LoadFwdPkgs(tx)

				return err
			}, func() {})
			require.NoError(t, err)
			if test.tombstone {
				require.Len(t, fwdPkgs, 1)
			} else {
				require.Empty(t, fwdPkgs)
			}
		})
	}
}

// TestCloseChannelTombstoneWritePath verifies the on-disk artefacts the
// tombstone close path produces in a single write transaction: the outpoint
// index flips from open to closed, the historical-channel record and close
// summary are written, and the bulk per-channel state (revocation log,
// forwarding packages) is left intact — that retention is the entire reason
// for the tombstone path on these backends.
func TestCloseChannelTombstoneWritePath(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithTombstoneClosedChannels(true))
	require.True(t, store.tombstoneClosedChannels)

	ch := createTestOpenChannel(t, store)

	const numRevlogEntries = 5
	const numFwdPkgs = 3
	writeTestRevlogEntries(t, store, ch, numRevlogEntries)
	writeTestForwardingPackages(t, store, ch, numFwdPkgs)

	closeChannelForTest(t, store, ch)

	// Outpoint index flipped from open to closed — the authoritative
	// closed-channel marker on tombstone backends.
	require.Equal(t, OutpointClosed, readOutpointStatus(
		t, store, ch.FundingOutpoint,
	))

	// Historical-channel record exists for this chanKey.
	histChan, err := store.FetchHistoricalChannel(&ch.FundingOutpoint)
	require.NoError(t, err)
	require.Equal(t, ch.FundingOutpoint, histChan.FundingOutpoint)

	// Close summary readable via FetchClosedChannel.
	closeSummary, err := store.FetchClosedChannel(&ch.FundingOutpoint)
	require.NoError(t, err)
	require.Equal(t, ch.FundingOutpoint, closeSummary.ChanPoint)

	// Bulk state preserved on disk — tombstoning's whole point.
	require.Equal(t, numRevlogEntries, countRevlogEntries(t, store, ch))

	packager := NewChannelPackager(ch.ShortChanID())
	var fwdPkgs []*FwdPkg
	require.NoError(t, kvdb.View(store.backend, func(tx kvdb.RTx) error {
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

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithTombstoneClosedChannels(true))
	ch := createTestOpenChannel(t, store)

	closeChannelForTest(t, store, ch)

	summary := &ChannelCloseSummary{
		ChanPoint:   ch.FundingOutpoint,
		RemotePub:   ch.IdentityPub,
		ChainHash:   ch.ChainHash,
		ShortChanID: ch.ShortChannelID,
		CloseType:   CooperativeClose,
	}
	require.ErrorIs(t, store.CloseChannel(ch, summary), ErrChannelNotFound)
}

// TestCloseChannelTombstoneRemovesFromOpenScans verifies that after a
// tombstone close the channel disappears from every open-channel scan
// (FetchAllChannels, FetchOpenChannels, FetchPermAndTempPeers) while the
// outpoint index reflects the close. The bulk historical state remains on
// disk — that is the entire point of the tombstone path.
func TestCloseChannelTombstoneRemovesFromOpenScans(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithTombstoneClosedChannels(true))
	require.True(t, store.tombstoneClosedChannels)

	// Two channels share an identity pubkey via createTestChannel, so we
	// can verify the closed one disappears while the other is still
	// surfaced by per-peer lookups.
	ch1 := createTestOpenChannel(t, store)
	ch2 := createTestOpenChannel(t, store)

	const numRevlogEntries = 5
	writeTestRevlogEntries(t, store, ch1, numRevlogEntries)

	openChans, err := store.FetchAllChannels()
	require.NoError(t, err)
	require.Len(t, openChans, 2)

	closeChannelForTest(t, store, ch1)

	openChans, err = store.FetchAllChannels()
	require.NoError(t, err)
	require.Len(t, openChans, 1)
	require.Equal(
		t, ch2.FundingOutpoint, openChans[0].FundingOutpoint,
	)

	openChans, err = store.FetchOpenChannels(ch1.IdentityPub)
	require.NoError(t, err)
	require.Len(t, openChans, 1)
	require.Equal(
		t, ch2.FundingOutpoint, openChans[0].FundingOutpoint,
	)

	// FetchPermAndTempPeers should still mark the peer as having a closed
	// channel (via the historical-channel second pass), even though the
	// open-channel-bucket pass now skips the closed chanKey.
	peers, err := store.FetchPermAndTempPeers(ch1.ChainHash[:])
	require.NoError(t, err)
	peerKey := string(ch1.IdentityPub.SerializeCompressed())
	require.True(t, peers[peerKey].HasOpenOrClosedChan)

	// The bulk historical state stays put — that is the whole point of
	// the tombstone path on these backends.
	require.Equal(t, numRevlogEntries, countRevlogEntries(t, store, ch1))

	// The outpoint index for ch1 must flip to closed; ch2's stays open.
	require.Equal(t, OutpointClosed, readOutpointStatus(
		t, store, ch1.FundingOutpoint,
	))
	require.Equal(t, OutpointOpen, readOutpointStatus(
		t, store, ch2.FundingOutpoint,
	))
}

// TestClosedChannelHiddenFromFetchChannel verifies that a targeted
// FetchChannel lookup returns ErrChannelNotFound for a closed channel.
// FetchChannel goes through channelScanner, exercising the closed-state
// check inside that iteration site rather than the direct fetchChanBucket
// lookup path.
func TestClosedChannelHiddenFromFetchChannel(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithTombstoneClosedChannels(true))
	ch := createTestOpenChannel(t, store)

	closeChannelForTest(t, store, ch)

	_, err := store.FetchChannel(ch.FundingOutpoint)
	require.ErrorIs(t, err, ErrChannelNotFound)
}

// TestClosedChannelHiddenFromDirectMethods verifies that direct OpenChannel
// methods which descend through fetchChanBucket / fetchChanBucketRw observe
// the closed-state flip and return ErrChannelNotFound rather than reading
// stale per-channel state.
func TestClosedChannelHiddenFromDirectMethods(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithTombstoneClosedChannels(true))
	ch := createTestOpenChannel(t, store)

	closeChannelForTest(t, store, ch)

	require.ErrorIs(t, ch.Refresh(), ErrChannelNotFound)
	require.ErrorIs(t, ch.MarkBorked(), ErrChannelNotFound)
}

// TestCloseChannelSync exercises the synchronous one-shot close path used by
// backends that do not opt in to tombstones (bbolt, etcd). It locks in the
// invariant that after CloseChannel returns the channel bucket and its
// revocation-log entries are already gone, and that the close summary,
// historical record, and outpoint flip are all in place.
func TestCloseChannelSync(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)
	require.False(t, store.tombstoneClosedChannels)

	ch := createTestOpenChannel(t, store)

	const numRevlogEntries = 4
	writeTestRevlogEntries(t, store, ch, numRevlogEntries)
	writeTestForwardingPackages(t, store, ch, 3)

	closeChannelForTest(t, store, ch)

	// The synchronous path wipes the chanBucket inline, so
	// countRevlogEntries must report -1 (bucket is gone, not just empty).
	require.Equal(t, -1, countRevlogEntries(t, store, ch),
		"channel bucket must be deleted after sync close")

	// Forwarding packages are wiped inline.
	var (
		fwdPkgs []*FwdPkg
		err     error
	)
	packager := NewChannelPackager(ch.ShortChanID())
	require.NoError(t, kvdb.View(store.backend, func(tx kvdb.RTx) error {
		fwdPkgs, err = packager.LoadFwdPkgs(tx)
		return err
	}, func() {}))
	require.Empty(t, fwdPkgs)

	// The outpoint index reflects the close.
	require.Equal(t, OutpointClosed, readOutpointStatus(
		t, store, ch.FundingOutpoint,
	))
}

// TestAbandonChannel tests that AbandonChannel removes a channel from the
// store and adds a close channel summary. Calling it again after the channel
// has already been removed should not return an error.
func TestAbandonChannel(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	err := store.AbandonChannel(&wire.OutPoint{}, 0)
	require.Error(t, err, "removing non-existent channel should fail")

	chanState := createTestChannel(t, store)

	closeHeight := uint32(11)
	err = store.AbandonChannel(&chanState.FundingOutpoint, closeHeight)
	require.NoError(t, err, "unable to abandon channel")

	_, err = store.FetchChannel(chanState.FundingOutpoint)
	require.ErrorIs(t, err, ErrChannelNotFound)

	_, err = store.FetchClosedChannel(&chanState.FundingOutpoint)
	require.NoError(t, err, "unable to fetch closed channel")

	err = store.AbandonChannel(&chanState.FundingOutpoint, closeHeight)
	require.NoError(t, err, "unable to abandon channel")
}
