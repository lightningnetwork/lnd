package chainntnfs_test

import (
	"crypto/sha256"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestConfirmationRegistrationOrder ensures that all subscribers receive a
// historical confirmation regardless of their registration order. A late
// subscriber with an earlier height hint than an already-dispatched scan
// causes the notifier to lower the persisted hint and dispatch a
// supplemental scan of the uncovered prefix. Each subcase below exercises a
// different ordering of the narrow scan (the original, later-hint scan) and
// the prefix scan (the supplemental, earlier-hint scan) relative to each
// other and to a notifier restart. Without the fix under test, the earlier
// subscriber's historical range could be lost or scanned incompletely
// depending on that ordering, causing it to miss a confirmation that
// occurred inside the prefix.
func TestConfirmationRegistrationOrder(t *testing.T) {
	testCases := []struct {
		name                string
		completeBeforeEarly bool
		prefixFirst         bool
		restartBeforePrefix bool
	}{
		{
			// The narrow scan completes before the earlier
			// subscriber even registers, so the prefix scan is
			// the only one outstanding when both complete.
			name:                "narrow scan already complete",
			completeBeforeEarly: true,
		},
		{
			// The narrow scan's callback returns before the
			// prefix scan's, exercising the default completion
			// order.
			name: "narrow scan completes first",
		},
		{
			// The prefix scan's callback returns before the
			// narrow scan's, the reverse of the default order.
			name:        "prefix scan completes first",
			prefixFirst: true,
		},
		{
			// The notifier restarts after the prefix obligation
			// is persisted but before the prefix scan completes,
			// verifying the obligation survives the restart.
			name:                "prefix survives restart",
			completeBeforeEarly: true,
			restartBeforePrefix: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testConfirmationRegistrationOrder(
				t, testCase.completeBeforeEarly,
				testCase.prefixFirst,
				testCase.restartBeforePrefix,
			)
		})
	}
}

// testConfirmationRegistrationOrder drives the scenario shared by every
// subcase of TestConfirmationRegistrationOrder. A transaction confirms, and
// the notifier restarts with no subscribers. A "late" subscriber registers
// with a height hint at the restart height, dispatching a narrow scan that
// starts after the confirmation. A second, "early" subscriber then
// registers with a height hint at the confirmation height, which is earlier
// than the narrow scan's start height. That triggers a supplemental scan of
// the uncovered prefix [minedHeight, restartHeight-1], the range the narrow
// scan never covered.
//
// completeBeforeEarly, when set, completes the narrow scan before the early
// subscriber registers, so only the prefix scan is outstanding once both
// subscribers are present. prefixFirst, when set, completes the prefix scan
// before the narrow scan; otherwise the narrow scan (or, if
// completeBeforeEarly is unset, its completion inline with registration)
// finishes first. restartBeforePrefix, when set, restarts the notifier
// after the prefix obligation is persisted to the hint cache but before the
// prefix scan itself completes, verifying that the obligation is not lost
// across the restart.
//
// In every subcase, both subscribers must end up with a confirmation at
// minedHeight once their scans finish and the chain advances. This is the
// property the registration-order fix restores: a bug here would cause the
// early subscriber to receive no confirmation, since its historical range
// would either not be scanned, or be scanned incorrectly, depending on scan
// and restart ordering.
func testConfirmationRegistrationOrder(t *testing.T, completeBeforeEarly,
	prefixFirst, restartBeforePrefix bool) {

	const (
		minedHeight   = uint32(100)
		restartHeight = uint32(103)
	)

	// Build a transaction and the block it will be mined in.
	program := sha256.Sum256([]byte("confirmation-registration-order"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	txid := tx.TxHash()
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})

	// Register an initial subscriber, complete its historical scan with
	// no match, then mine the transaction and tear the notifier down.
	// This leaves the cache holding a confirmation at minedHeight with
	// no subscribers left to observe it directly.
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		minedHeight-1, chainntnfs.ReorgSafetyLimit, cache, cache,
	)

	initial, err := n.RegisterConf(
		&txid, script, 1, minedHeight-1,
	)
	require.NoError(t, err)
	require.NoError(t, n.UpdateConfDetails(
		initial.HistoricalDispatch.ConfRequest, nil,
	))
	require.NoError(t, n.ConnectTip(block, minedHeight))
	require.NoError(t, n.NotifyHeight(minedHeight))
	n.TearDown()

	// Restart the notifier at restartHeight and register the late
	// subscriber. Its height hint of restartHeight is after minedHeight,
	// so its narrow historical scan will not cover the block that
	// contains the confirmation.
	n = chainntnfs.NewTxNotifier(
		restartHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(func() {
		n.TearDown()
	})

	late, err := n.RegisterConf(&txid, script, 6, restartHeight)
	require.NoError(t, err)
	require.NotNil(t, late.HistoricalDispatch)

	// scan completes the historical dispatch for registration,
	// delivering the mined confirmation only if minedHeight falls within
	// the dispatch's scanned range.
	scan := func(registration *chainntnfs.ConfRegistration) {
		dispatch := registration.HistoricalDispatch
		require.NotNil(t, dispatch)

		var details *chainntnfs.TxConfirmation
		if dispatch.StartHeight <= minedHeight &&
			minedHeight <= dispatch.EndHeight {

			details = &chainntnfs.TxConfirmation{
				BlockHash:   block.Hash(),
				BlockHeight: minedHeight,
				Tx:          tx,
			}
		}

		require.NoError(t, n.UpdateConfDetails(
			dispatch.ConfRequest, details,
		))
	}

	// Optionally complete the late subscriber's narrow scan before the
	// early subscriber ever registers. The cached hint should then sit
	// at restartHeight, since nothing has lowered it yet.
	if completeBeforeEarly {
		scan(late)
		hint, err := cache.confirmHeight(
			late.HistoricalDispatch.ConfRequest,
		)
		require.NoError(t, err)
		require.Equal(t, restartHeight, hint)
	}

	// Register the early subscriber with a height hint at minedHeight.
	// Since that hint is earlier than the narrow scan's start height,
	// the notifier must dispatch a supplemental scan covering exactly
	// the uncovered prefix up to, but not including, the narrow scan's
	// start.
	early, err := n.RegisterConf(&txid, script, 3, minedHeight)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)
	require.Equal(t, minedHeight, early.HistoricalDispatch.StartHeight)
	require.Equal(t, restartHeight-1, early.HistoricalDispatch.EndHeight)
	require.True(t, early.HistoricalDispatch.Supplemental)

	// The earlier range must be durable as soon as it is accepted. If
	// the notifier restarts before the prefix completes, the cache must
	// cause the replacement subscription to scan that range again.
	switch {
	case restartBeforePrefix:
		// Confirm the cached hint cannot skip the prefix, since its
		// origin is above the early subscriber's hint, then restart
		// the notifier before the prefix scan completes.
		// Re-registering both subscribers after the restart must
		// reproduce the same prefix obligation, since the in-memory
		// scan state was lost.
		require.Equal(t, minedHeight, cache.confRestartStart(
			early.HistoricalDispatch.ConfRequest, minedHeight,
		))

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			restartHeight, chainntnfs.ReorgSafetyLimit,
			cache, cache,
		)

		early, err = n.RegisterConf(&txid, script, 3, minedHeight)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, minedHeight, early.HistoricalDispatch.StartHeight,
		)
		require.Equal(
			t, restartHeight, early.HistoricalDispatch.EndHeight,
		)
		require.False(t, early.HistoricalDispatch.Supplemental)

		late, err = n.RegisterConf(&txid, script, 6, restartHeight)
		require.NoError(t, err)
		require.Nil(t, late.HistoricalDispatch)
		scan(early)
	case prefixFirst:
		// Complete the prefix scan before the narrow scan, the
		// reverse of the default completion order.
		scan(early)
		scan(late)

	default:
		// Complete the narrow scan, unless it was already completed
		// above via completeBeforeEarly, then complete the prefix
		// scan.
		if !completeBeforeEarly {
			scan(late)
			hint, err := cache.confirmHeight(
				late.HistoricalDispatch.ConfRequest,
			)
			require.NoError(t, err)
			require.Equal(t, minedHeight, hint)
		}
		scan(early)
	}

	// Regardless of completion order, the final cached hint must reflect
	// the earlier subscriber's range rather than being clobbered by
	// whichever scan happened to finish last.
	hint, err := cache.confirmHeight(
		early.HistoricalDispatch.ConfRequest,
	)
	require.NoError(t, err)
	require.Equal(t, minedHeight, hint)

	// Advance the chain well past both subscribers' required
	// confirmation depths.
	for height := restartHeight + 1; height <= minedHeight+10; height++ {
		require.NoError(t, n.ConnectTip(
			btcutil.NewBlock(&wire.MsgBlock{}), height,
		))
		require.NoError(t, n.NotifyHeight(height))
	}

	// Both subscribers must have received the historical confirmation,
	// regardless of how their scans and any restart were interleaved.
	select {
	case details := <-early.Event.Confirmed:
		require.Equal(t, minedHeight, details.BlockHeight)
	default:
		t.Fatal("early subscriber missed historical confirmation")
	}

	select {
	case details := <-late.Event.Confirmed:
		require.Equal(t, minedHeight, details.BlockHeight)
	default:
		t.Fatal("late subscriber missed historical confirmation")
	}
}

// TestConfirmationRegistrationOrderProperty asserts that subscriber and scan
// completion order cannot change the historical range covered for a
// request. Unlike TestConfirmationRegistrationOrder, which walks four fixed
// two-subscriber scenarios, this test draws a random chain height, a random
// set of two to six subscriber height hints (including a distinguished
// minimum hint), and a random permutation for completing their resulting
// scans. It checks two invariants: the dispatched historical ranges must
// tile [minHint, tipHeight] exactly once each, and every subscriber must end
// up with exactly one confirmation once all scans complete, regardless of
// completion order. A bug in how supplemental prefix scans are sized or
// merged would surface here as a gap, an overlap, or a subscriber that never
// gets, or is delivered twice, its confirmation.
func TestConfirmationRegistrationOrderProperty(t *testing.T) {
	program := sha256.Sum256([]byte("confirmation-order-property"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	txid := tx.TxHash()
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})

	rapid.Check(t, func(t *rapid.T) {
		// Draw a chain height, a minimum height hint, and a set of
		// subscriber hints, with the minimum forced into one
		// subscriber's slot at or after the first. Other hints are
		// drawn no lower than minHint+1, so they may create
		// additional descending prefixes or duplicate an existing
		// hint, but never tie for the minimum.
		tipHeight := rapid.Uint32Range(3, 1000).Draw(
			t, "tip_height",
		)
		minHint := rapid.Uint32Range(1, tipHeight-1).Draw(
			t, "min_hint",
		)
		numSubscribers := rapid.IntRange(2, 6).Draw(
			t, "num_subscribers",
		)
		minHintIndex := rapid.IntRange(1, numSubscribers-1).Draw(
			t, "min_hint_index",
		)

		// Place the minimum after the first subscriber. Other hints can
		// create additional descending prefixes or duplicates.
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

		minedHeight := rapid.Uint32Range(minHint, tipHeight).Draw(
			t, "mined_height",
		)

		// Register every subscriber in order, collecting the
		// resulting historical dispatches. Subscribers whose hint is
		// covered by an already-dispatched scan receive no dispatch
		// of their own.
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		defer n.TearDown()

		events := make([]*chainntnfs.ConfirmationEvent, 0, len(hints))
		dispatches := make(
			[]*chainntnfs.HistoricalConfDispatch, 0, len(hints),
		)
		for _, hint := range hints {
			registration, err := n.RegisterConf(
				&txid, script, 1, hint,
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

		// Complete the dispatches in a randomly drawn order,
		// delivering the mined confirmation only once, to the
		// dispatch whose range actually contains minedHeight.
		completionOrder := rapid.Permutation(dispatches).Draw(
			t, "completion_order",
		)
		found := false
		for _, dispatch := range completionOrder {
			var details *chainntnfs.TxConfirmation
			if dispatch.StartHeight <= minedHeight &&
				minedHeight <= dispatch.EndHeight {

				details = &chainntnfs.TxConfirmation{
					BlockHash:   block.Hash(),
					BlockHeight: minedHeight,
					Tx:          tx,
				}
				found = true
			}

			require.NoError(t, n.UpdateConfDetails(
				dispatch.ConfRequest, details,
			))

			// An empty partial result cannot discard a prefix whose
			// completion is still outstanding.
			if !found {
				start := cache.confRestartStart(
					dispatch.ConfRequest, minHint,
				)
				require.Equal(t, minHint, start)
			}
		}

		// Every subscriber must receive exactly one confirmation,
		// regardless of the order in which the dispatches above
		// completed.
		for _, event := range events {
			select {
			case details := <-event.Confirmed:
				require.Equal(
					t, minedHeight, details.BlockHeight,
				)
			default:
				t.Fatal("subscriber missed confirmation")
			}

			select {
			case <-event.Confirmed:
				t.Fatal("subscriber received duplicate")
			default:
			}
		}
	})
}
