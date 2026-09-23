package chainntnfs_test

import (
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestSpendRegistrationEarlierHintAfterCompletion asserts that an earlier hint
// still triggers a prefix scan once the set's initial scan has completed and
// the set is watching at tip. After both scans report empty results, the hint
// advances with the tip again.
func TestSpendRegistrationEarlierHintAfterCompletion(t *testing.T) {
	const (
		tipHeight = uint32(200)
		lateHint  = uint32(100)
		earlyHint = uint32(50)
	)

	outpoint, script, _ := staleHintSpend("spend-earlier-hint-complete")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Complete the initial scan without a spend so the set is watching
	// at tip with its hint at the current height.
	late, err := n.RegisterSpend(&outpoint, script, lateHint)
	require.NoError(t, err)
	initial := late.HistoricalDispatch
	require.NotNil(t, initial)
	require.NoError(t, n.UpdateSpendDetails(initial, nil))
	assertSpendHint(t, cache, initial.SpendRequest, tipHeight)

	// A subscriber with an earlier hint needs the range the initial scan
	// never covered.
	early, err := n.RegisterSpend(&outpoint, script, earlyHint)
	require.NoError(t, err)
	prefix := early.HistoricalDispatch
	require.NotNil(t, prefix)
	require.True(t, prefix.Supplemental)
	require.Equal(t, earlyHint, prefix.StartHeight)
	require.Equal(t, lateHint-1, prefix.EndHeight)
	assertSpendRestart(t, cache, prefix.SpendRequest, earlyHint)

	// Tip updates are suspended until the prefix scan reports.
	advanceEmptyChain(t, n, tipHeight+1, tipHeight+1)
	assertSpendRestart(t, cache, prefix.SpendRequest, earlyHint)

	// Once it reports an empty result, the hint follows the tip again.
	require.NoError(t, n.UpdateSpendDetails(prefix, nil))
	assertSpendHint(t, cache, prefix.SpendRequest, tipHeight+1)
	advanceEmptyChain(t, n, tipHeight+2, tipHeight+2)
	assertSpendHint(t, cache, prefix.SpendRequest, tipHeight+2)

	assertNoSpend(t, late.Event)
	assertNoSpend(t, early.Event)
}

// TestSpendRegistrationOrderProperty asserts that subscriber order and scan
// completion order cannot change the historical range covered for a spend
// request. The dispatched ranges must partition the blocks from the earliest
// hint to the tip, and every subscriber must receive the spend exactly once.
func TestSpendRegistrationOrderProperty(t *testing.T) {
	outpoint, script, tx := staleHintSpend("spend-order-property")
	spenderHash := tx.TxHash()

	rapid.Check(t, func(t *rapid.T) {
		tipHeight := rapid.Uint32Range(3, 1000).Draw(t, "tip_height")
		minHint := rapid.Uint32Range(1, tipHeight-1).Draw(
			t, "min_hint",
		)
		numSubscribers := rapid.IntRange(2, 6).Draw(
			t, "num_subscribers",
		)
		minHintIndex := rapid.IntRange(1, numSubscribers-1).Draw(
			t, "min_hint_index",
		)

		// Place the minimum after the first subscriber so at least one
		// supplemental scan is required. Other hints can create
		// additional descending prefixes or duplicates.
		hints := make([]uint32, numSubscribers)
		for i := range hints {
			if i == minHintIndex {
				hints[i] = minHint
				continue
			}

			hints[i] = rapid.Uint32Range(minHint+1, tipHeight).Draw(
				t, "height_hint_"+string(rune('a'+i)),
			)
		}

		spendHeight := rapid.Uint32Range(minHint, tipHeight).Draw(
			t, "spend_height",
		)

		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		defer n.TearDown()

		// Register every subscriber and collect the dispatches.
		events := make([]*chainntnfs.SpendEvent, 0, len(hints))
		dispatches := make(
			[]*chainntnfs.HistoricalSpendDispatch, 0, len(hints),
		)
		for _, hint := range hints {
			registration, err := n.RegisterSpend(
				&outpoint, script, hint,
			)
			require.NoError(t, err)
			events = append(events, registration.Event)

			if registration.HistoricalDispatch != nil {
				dispatches = append(
					dispatches,
					registration.HistoricalDispatch,
				)
			}
		}

		// The dispatched ranges must be a gapless, non-overlapping
		// partition of every block promised by the earliest subscriber.
		sort.Slice(dispatches, func(i, j int) bool {
			return dispatches[i].StartHeight <
				dispatches[j].StartHeight
		})
		require.NotEmpty(t, dispatches)
		require.Equal(t, minHint, dispatches[0].StartHeight)
		require.Equal(
			t, tipHeight, dispatches[len(dispatches)-1].EndHeight,
		)
		for i := 1; i < len(dispatches); i++ {
			require.Equal(
				t, dispatches[i-1].EndHeight+1,
				dispatches[i].StartHeight,
			)
		}

		// Only the scan that ends at the tip is an initial scan. Every
		// prefix below it keeps its fixed end height.
		for i, dispatch := range dispatches {
			require.Equal(
				t, i != len(dispatches)-1,
				dispatch.Supplemental,
			)
		}

		// Complete the scans in a random order. Only the scan whose
		// range contains the spend reports it.
		completionOrder := rapid.Permutation(dispatches).Draw(
			t, "completion_order",
		)
		found := false
		for _, dispatch := range completionOrder {
			var details *chainntnfs.SpendDetail
			if dispatch.StartHeight <= spendHeight &&
				spendHeight <= dispatch.EndHeight {

				details = &chainntnfs.SpendDetail{
					SpentOutPoint:  &outpoint,
					SpenderTxHash:  &spenderHash,
					SpendingTx:     tx,
					SpendingHeight: int32(spendHeight),
				}
				found = true
			}

			require.NoError(t, n.UpdateSpendDetails(
				dispatch, details,
			))

			// An empty partial result cannot discard a prefix whose
			// completion is still outstanding.
			if !found {
				start := cache.spendRestartStart(
					dispatch.SpendRequest, minHint,
				)
				require.Equal(t, minHint, start)
			}
		}

		for _, event := range events {
			select {
			case details := <-event.Spend:
				require.Equal(
					t, int32(spendHeight),
					details.SpendingHeight,
				)
			default:
				t.Fatal("subscriber missed spend")
			}

			select {
			case <-event.Spend:
				t.Fatal("subscriber received duplicate")
			default:
			}
		}
	})
}

// TestSpendProgressPersistence asserts that a historical scan's progress is
// persisted by the notifier with its next hint update, tagged with the set's
// coverage as its origin. Progress is dropped once the set learns of a spend
// or loses its subscribers, and progress from a scan that no longer covers
// the lowest range is ignored. Writing it from the scan itself instead could
// claim blocks past a spend the notifier had already found at tip, recreate a
// hint no subscriber owns, or credit one scan's origin to another.
func TestSpendProgressPersistence(t *testing.T) {
	const (
		tipHeight = uint32(200)
		lateHint  = uint32(100)
		earlyHint = uint32(50)
	)

	outpoint, script, tx := staleHintSpend("spend-progress-persistence")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	late, err := n.RegisterSpend(&outpoint, script, lateHint)
	require.NoError(t, err)
	initial := late.HistoricalDispatch
	require.NotNil(t, initial)
	request := initial.SpendRequest
	height := tipHeight

	// connect advances the chain by one empty block and returns the
	// cached hint, which is where recorded progress is persisted.
	connect := func() chainntnfs.HeightHint {
		t.Helper()
		height++
		advanceEmptyChain(t, n, height, height)
		hint, err := cache.QuerySpendHint(request)
		require.NoError(t, err)

		return hint
	}

	// Recorded progress is persisted with the next block.
	initial.Progress.Update(150)
	require.Equal(t, chainntnfs.HeightHint{
		Height: 150, Origin: lateHint,
	}, connect())

	// A supplemental scan lowers the set's coverage. Only its progress,
	// under the lower origin, is persisted from then on.
	early, err := n.RegisterSpend(&outpoint, script, earlyHint)
	require.NoError(t, err)
	prefix := early.HistoricalDispatch
	require.NotNil(t, prefix)
	initial.Progress.Update(190)
	connect()
	assertSpendRestart(t, cache, request, earlyHint)
	prefix.Progress.Update(60)
	require.Equal(t, chainntnfs.HeightHint{
		Height: 60, Origin: earlyHint,
	}, connect())

	// Once the set learns of a spend at tip, its hint follows the spend and
	// later progress is ignored.
	height++
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), height))
	require.NoError(t, n.NotifyHeight(height))
	spendHeight := height
	prefix.Progress.Update(80)
	require.Equal(t, spendHeight, connect().Height)

	// After every subscriber cancels, nothing more is persisted.
	late.Event.Cancel()
	early.Event.Cancel()
	released, err := cache.QuerySpendHint(request)
	require.NoError(t, err)
	prefix.Progress.Update(height)
	require.Equal(t, released, connect())
}

// TestSpendProgressAfterCancel asserts that progress recorded by a scan that
// is still outstanding after its only subscriber canceled is never persisted.
// No subscriber owns the hint anymore, and nothing tracks reorgs for it.
func TestSpendProgressAfterCancel(t *testing.T) {
	const tipHeight = uint32(200)

	outpoint, script, _ := staleHintSpend("spend-progress-after-cancel")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	registration, err := n.RegisterSpend(&outpoint, script, 100)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	registration.Event.Cancel()

	dispatch.Progress.Update(150)
	advanceEmptyChain(t, n, tipHeight+1, tipHeight+1)
	_, err = cache.QuerySpendHint(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
}

// assertSpendRestart asserts that the first spend registration after a
// restart, with the given height hint, would begin scanning at that hint.
func assertSpendRestart(t require.TestingT, cache *mockHintCache,
	request chainntnfs.SpendRequest, heightHint uint32) {

	require.Equal(
		t, heightHint, cache.spendRestartStart(request, heightHint),
	)
}

// assertSpendHint asserts that the cached spend hint for the request begins
// at the expected height.
func assertSpendHint(t require.TestingT, cache chainntnfs.SpendHintCache,
	request chainntnfs.SpendRequest, expected uint32) {

	hint, err := cache.QuerySpendHint(request)
	require.NoError(t, err)
	require.Equal(t, expected, hint.Height)
}
