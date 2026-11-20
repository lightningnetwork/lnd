package invoices

import (
	"errors"
	"maps"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockFinalHtlcDB arranges storage responses without duplicating the
// production outcome store or its channel-lifecycle rules.
type mockFinalHtlcDB struct {
	mock.Mock
}

// newMockFinalHtlcDB verifies configured interactions once after the test.
func newMockFinalHtlcDB(t *testing.T) *mockFinalHtlcDB {
	t.Helper()

	db := &mockFinalHtlcDB{}
	t.Cleanup(func() {
		db.AssertExpectations(t)
	})

	return db
}

// LookupFinalHtlc returns arranged point-lookup evidence, keeping a missing
// record distinct from an actual success or failure.
func (m *mockFinalHtlcDB) LookupFinalHtlc(chanID lnwire.ShortChannelID,
	htlcIndex uint64) (*chanstate.FinalHtlcInfo, error) {

	args := m.Called(chanID, htlcIndex)
	info, _ := args.Get(0).(*chanstate.FinalHtlcInfo)

	return info, args.Error(1)
}

// FetchAllChannels returns current commitments so incoming HTLCs still on
// either side cannot be inferred settled prematurely.
func (m *mockFinalHtlcDB) FetchAllChannels() ([]*chanstate.OpenChannel,
	error) {

	args := m.Called()
	channels, _ := args.Get(0).([]*chanstate.OpenChannel)

	return channels, args.Error(1)
}

// FetchClosedChannels returns arranged close summaries so pending closes
// remain distinguishable from fully completed channel resolution.
func (m *mockFinalHtlcDB) FetchClosedChannels(pendingOnly bool) (
	[]*chanstate.ChannelCloseSummary, error) {

	args := m.Called(pendingOnly)
	closes, _ := args.Get(0).([]*chanstate.ChannelCloseSummary)

	return closes, args.Error(1)
}

// FetchHistoricalChannel supplies the archived status that a close summary
// omits, so restored snapshots cannot be mistaken for payment evidence.
func (m *mockFinalHtlcDB) FetchHistoricalChannel(outPoint *wire.OutPoint) (
	*chanstate.OpenChannel, error) {

	args := m.Called(outPoint)
	channel, _ := args.Get(0).(*chanstate.OpenChannel)

	return channel, args.Error(1)
}

// Keep the double aligned with the complete source-read contract.
var _ finalHtlcDB = (*mockFinalHtlcDB)(nil)

// TestFinalHtlcSourceLookup checks which recorded and channel-state evidence
// can establish the final outcome of an incoming HTLC already released.
func TestFinalHtlcSourceLookup(t *testing.T) {
	t.Parallel()

	// Arrange: Cover both current commitments, zero-conf identities and
	// pending/resolved closes, untrusted snapshots and abandoned contracts.
	// Recorded outcomes remain trusted despite these snapshot gaps.
	var (
		openScid     = lnwire.NewShortChanIDFromInt(1)
		zeroConfScid = lnwire.NewShortChanIDFromInt(2)
		realScid     = lnwire.NewShortChanIDFromInt(3)
		closingScid  = lnwire.NewShortChanIDFromInt(4)
		resolvedScid = lnwire.NewShortChanIDFromInt(5)
		unknownScid  = lnwire.NewShortChanIDFromInt(6)
		restoredOpen = lnwire.NewShortChanIDFromInt(7)
		restoredEnd  = lnwire.NewShortChanIDFromInt(8)
		dataLossOpen = lnwire.NewShortChanIDFromInt(9)
		dataLossEnd  = lnwire.NewShortChanIDFromInt(10)
		abandonScid  = lnwire.NewShortChanIDFromInt(11)
		remoteScid   = lnwire.NewShortChanIDFromInt(12)
	)

	key := func(scid lnwire.ShortChannelID,
		htlcID uint64) CircuitKey {

		return CircuitKey{
			ChanID: scid,
			HtlcID: htlcID,
		}
	}
	incoming := func(htlcID uint64) chanstate.HTLC {
		return chanstate.HTLC{
			HtlcIndex: htlcID,
			Incoming:  true,
		}
	}

	// HTLCs 1 and 2 remain on opposite commitments. Outgoing HTLC 3
	// must not mask an absent incoming HTLC with the same index. Index4
	// is the next incoming HTLC, so this snapshot cannot prove its removal.
	openChannel := &chanstate.OpenChannel{
		ShortChannelID: openScid,
		LocalCommitment: chanstate.ChannelCommitment{
			RemoteHtlcIndex: 4,
			Htlcs: []chanstate.HTLC{
				incoming(1),
				{
					HtlcIndex: 3,
					Incoming:  false,
				},
			},
		},
		RemoteCommitment: chanstate.ChannelCommitment{
			Htlcs: []chanstate.HTLC{
				incoming(2),
			},
		},
	}

	// Both zero-conf identities refer to a snapshot that has seen index1.
	zeroConfChannel := &chanstate.OpenChannel{
		ShortChannelID: zeroConfScid,
		ChanType:       chanstate.ZeroConfBit,
		LocalCommitment: chanstate.ChannelCommitment{
			RemoteHtlcIndex: 2,
		},
	}
	zeroConfChannel.SetConfirmedScidForStore(realScid)

	// Restored status survives closure in historical channel data, but
	// neither empty snapshot establishes that a released HTLC was paid.
	// A seen index keeps the status guard independent of the counter check.
	restoredChannel := &chanstate.OpenChannel{
		ShortChannelID: restoredOpen,
		LocalCommitment: chanstate.ChannelCommitment{
			RemoteHtlcIndex: 2,
		},
	}
	restoredChannel.SetChannelStatusForStore(chanstate.ChanStatusRestored)
	resolvedSummary := &chanstate.ChannelCloseSummary{
		ShortChanID: resolvedScid,
		ChanPoint: wire.OutPoint{
			Index: 1,
		},
	}
	restoredSummary := &chanstate.ChannelCloseSummary{
		ShortChanID: restoredEnd,
		ChanPoint: wire.OutPoint{
			Index: 2,
		},
	}

	// A stale snapshot can omit a released incoming HTLC despite retaining
	// other HTLCs. Only recorded evidence can establish its collection.
	dataLossChannel := &chanstate.OpenChannel{
		ShortChannelID:   dataLossOpen,
		LocalCommitment:  openChannel.LocalCommitment,
		RemoteCommitment: openChannel.RemoteCommitment,
	}
	dataLossChannel.SetChannelStatusForStore(
		chanstate.ChanStatusLocalDataLoss,
	)
	dataLossSummary := &chanstate.ChannelCloseSummary{
		ShortChanID: dataLossEnd,
		ChanPoint: wire.OutPoint{
			Index: 3,
		},
	}

	// Abandonment archives incoming HTLCs without collecting them. Reuse
	// both commitments to cover each side and the outgoing-only index.
	abandonedChannel := &chanstate.OpenChannel{
		ShortChannelID:   abandonScid,
		LocalCommitment:  openChannel.LocalCommitment,
		RemoteCommitment: openChannel.RemoteCommitment,
	}
	abandonedSummary := &chanstate.ChannelCloseSummary{
		ShortChanID: abandonScid,
		CloseType:   chanstate.Abandoned,
		ChanPoint: wire.OutPoint{
			Index: 4,
		},
	}

	// Local add1 can precede the remote commitment that closes. Index0
	// was already removed from both; only its absence proves collection.
	remoteSummary := &chanstate.ChannelCloseSummary{
		ShortChanID: remoteScid,
		CloseType:   chanstate.RemoteForceClose,
		ChanPoint: wire.OutPoint{
			Index: 5,
		},
	}
	remoteChannel := &chanstate.OpenChannel{
		LocalCommitment: chanstate.ChannelCommitment{
			RemoteHtlcIndex: 2,
			Htlcs: []chanstate.HTLC{
				incoming(1),
			},
		},
		RemoteCommitment: chanstate.ChannelCommitment{
			RemoteHtlcIndex: 1,
		},
	}

	keys := []CircuitKey{
		key(openScid, 1), key(openScid, 2), key(openScid, 3),
		key(openScid, 4), key(openScid, 5), key(zeroConfScid, 1),
		key(realScid, 1), key(closingScid, 1), key(resolvedScid, 1),
		key(resolvedScid, 2), key(unknownScid, 1),
		key(restoredOpen, 1), key(restoredOpen, 2),
		key(restoredEnd, 1),
		key(dataLossOpen, 3), key(dataLossEnd, 3),
		key(abandonScid, 1), key(abandonScid, 2),
		key(abandonScid, 3), key(remoteScid, 0), key(remoteScid, 1),
	}
	db := newMockFinalHtlcDB(t)
	for _, k := range keys {
		if k == key(openScid, 5) {
			db.On("LookupFinalHtlc", k.ChanID, k.HtlcID).Return(
				&chanstate.FinalHtlcInfo{
					Settled:  false,
					Offchain: false,
				}, nil,
			).Once()

			continue
		}

		if k == key(restoredOpen, 2) {
			db.On("LookupFinalHtlc", k.ChanID, k.HtlcID).Return(
				&chanstate.FinalHtlcInfo{
					Settled:  true,
					Offchain: true,
				}, nil,
			).Once()

			continue
		}

		db.On("LookupFinalHtlc", k.ChanID, k.HtlcID).Return(
			nil, chanstate.ErrHtlcUnknown,
		).Once()
	}
	db.On("FetchAllChannels").Return([]*chanstate.OpenChannel{
		openChannel,
		zeroConfChannel,
		restoredChannel,
		dataLossChannel,
	}, nil).Once()
	db.On("FetchClosedChannels", false).Return(
		[]*chanstate.ChannelCloseSummary{
			{
				ShortChanID: closingScid,
				IsPending:   true,
			},
			resolvedSummary,
			restoredSummary,
			dataLossSummary,
			abandonedSummary,
			remoteSummary,
		}, nil,
	).Once()

	// The archived snapshot has seen index1, while index2 is still unseen.
	db.On("FetchHistoricalChannel", &resolvedSummary.ChanPoint).Return(
		&chanstate.OpenChannel{
			LocalCommitment: chanstate.ChannelCommitment{
				RemoteHtlcIndex: 2,
			},
		}, nil,
	).Times(2)
	db.On("FetchHistoricalChannel", &restoredSummary.ChanPoint).Return(
		restoredChannel, nil,
	).Once()
	db.On("FetchHistoricalChannel", &dataLossSummary.ChanPoint).Return(
		dataLossChannel, nil,
	).Once()
	db.On("FetchHistoricalChannel", &abandonedSummary.ChanPoint).Return(
		abandonedChannel, nil,
	).Times(3)
	db.On("FetchHistoricalChannel", &remoteSummary.ChanPoint).Return(
		remoteChannel, nil,
	).Times(2)
	source := NewFinalHtlcSource(db, nil)

	// Act: Read stored outcomes first, without treating a missing record
	// as success or scanning channel state during these point lookups.
	outcomes, err := source.LookupFinalHtlcs(keys)
	require.NoError(t, err)

	// Assert: The stored failure and restored-channel success are direct
	// evidence; inference must neither overwrite nor discard these records.
	require.Equal(t, map[CircuitKey]FinalHtlcOutcome{
		key(openScid, 5): {
			Key: key(openScid, 5),
		},
		key(restoredOpen, 2): {
			Key:      key(restoredOpen, 2),
			Settled:  true,
			Offchain: true,
		},
	}, outcomes)

	// Act: Infer only keys lacking a durable result, as missed-event
	// recovery does, and combine that evidence with the recorded failure.
	var remaining []CircuitKey
	for _, k := range keys {
		if _, ok := outcomes[k]; !ok {
			remaining = append(remaining, k)
		}
	}
	inferred, err := source.InferFinalHtlcs(remaining)
	require.NoError(t, err)
	maps.Copy(outcomes, inferred)

	settledOffchain := func(
		k CircuitKey) FinalHtlcOutcome {

		return FinalHtlcOutcome{
			Key:      k,
			Settled:  true,
			Offchain: true,
		}
	}

	// Assert: Unresolved or untrusted snapshots add no inferred success.
	// Recorded outcomes remain authoritative.
	expected := map[CircuitKey]FinalHtlcOutcome{
		key(openScid, 3): settledOffchain(key(openScid, 3)),
		key(openScid, 5): {
			Key: key(openScid, 5),
		},
		key(zeroConfScid, 1): settledOffchain(key(zeroConfScid, 1)),
		key(realScid, 1):     settledOffchain(key(realScid, 1)),
		key(resolvedScid, 1): {
			Key:     key(resolvedScid, 1),
			Settled: true,
		},
		key(restoredOpen, 2): settledOffchain(key(restoredOpen, 2)),
		key(abandonScid, 3): {
			Key:     key(abandonScid, 3),
			Settled: true,
		},
		key(remoteScid, 0): {
			Key:     key(remoteScid, 0),
			Settled: true,
		},
	}
	require.Equal(t, expected, outcomes)

	// Arrange: A closed summary also becomes untrusted when its archived
	// channel cannot be read, independently of a known restored shell.
	historyErr := errors.New("historical channel unavailable")
	db.On("FetchAllChannels").Return(nil, nil).Once()
	db.On("FetchClosedChannels", false).Return(
		[]*chanstate.ChannelCloseSummary{resolvedSummary}, nil,
	).Once()
	db.On("FetchHistoricalChannel", &resolvedSummary.ChanPoint).Return(
		nil, historyErr,
	).Once()

	// Act: Retry the resolved candidate without readable channel history.
	inferred, err = source.InferFinalHtlcs(
		[]CircuitKey{key(resolvedScid, 1)},
	)

	// Assert: Preserve the storage cause and leave payment unknown instead
	// of accepting the fully closed summary as evidence of collection.
	require.ErrorIs(t, err, historyErr)
	require.Empty(t, inferred)
}
