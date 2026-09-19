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
// historical confirmation regardless of their registration order.
func TestConfirmationRegistrationOrder(t *testing.T) {
	testCases := []struct {
		name                string
		completeBeforeEarly bool
		prefixFirst         bool
		restartBeforePrefix bool
	}{
		{
			name:                "narrow scan already complete",
			completeBeforeEarly: true,
		},
		{
			name: "narrow scan completes first",
		},
		{
			name:        "prefix scan completes first",
			prefixFirst: true,
		},
		{
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

func testConfirmationRegistrationOrder(t *testing.T, completeBeforeEarly,
	prefixFirst, restartBeforePrefix bool) {

	const (
		minedHeight   = uint32(100)
		restartHeight = uint32(103)
	)

	program := sha256.Sum256([]byte("confirmation-registration-order"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	txid := tx.TxHash()
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})

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

	n = chainntnfs.NewTxNotifier(
		restartHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(func() {
		n.TearDown()
	})

	late, err := n.RegisterConf(&txid, script, 6, restartHeight)
	require.NoError(t, err)
	require.NotNil(t, late.HistoricalDispatch)

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

	if completeBeforeEarly {
		scan(late)
		hint, err := cache.QueryConfirmHint(
			late.HistoricalDispatch.ConfRequest,
		)
		require.NoError(t, err)
		require.Equal(t, restartHeight, hint)
	}

	early, err := n.RegisterConf(&txid, script, 3, minedHeight)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)
	require.Equal(t, minedHeight, early.HistoricalDispatch.StartHeight)
	require.Equal(t, restartHeight-1, early.HistoricalDispatch.EndHeight)

	// The earlier range must be durable as soon as it is accepted. If
	// the notifier restarts before the prefix completes, the cache must
	// cause the replacement subscription to scan that range again.
	switch {
	case restartBeforePrefix:
		hint, err := cache.QueryConfirmHint(
			early.HistoricalDispatch.ConfRequest,
		)
		require.NoError(t, err)
		require.Equal(t, minedHeight, hint)

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

		late, err = n.RegisterConf(&txid, script, 6, restartHeight)
		require.NoError(t, err)
		require.Nil(t, late.HistoricalDispatch)
		scan(early)
	case prefixFirst:
		scan(early)
		scan(late)

	default:
		if !completeBeforeEarly {
			scan(late)
			hint, err := cache.QueryConfirmHint(
				late.HistoricalDispatch.ConfRequest,
			)
			require.NoError(t, err)
			require.Equal(t, minedHeight, hint)
		}
		scan(early)
	}

	hint, err := cache.QueryConfirmHint(
		early.HistoricalDispatch.ConfRequest,
	)
	require.NoError(t, err)
	require.Equal(t, minedHeight, hint)

	for height := restartHeight + 1; height <= minedHeight+10; height++ {
		require.NoError(t, n.ConnectTip(
			btcutil.NewBlock(&wire.MsgBlock{}), height,
		))
		require.NoError(t, n.NotifyHeight(height))
	}

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
// completion order cannot change the historical range covered for a request.
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
				hint, err := cache.QueryConfirmHint(
					dispatch.ConfRequest,
				)
				require.NoError(t, err)
				require.Equal(t, minHint, hint)
			}
		}

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

type confirmationLifecycleScenario struct {
	tipHeight         uint32
	hints             []uint32
	numConfs          []uint32
	cancelIndex       int
	cancelAfterConf   bool
	scanInterruptions int
	preMineBlocks     uint32
	reincludeDelay    uint32
}

func genLifecycleScenarios() *rapid.Generator[confirmationLifecycleScenario] {
	return rapid.Custom(func(t *rapid.T) confirmationLifecycleScenario {
		tipHeight := rapid.Uint32Range(3, 1000).Draw(t, "tip_height")
		minHint := rapid.Uint32Range(1, tipHeight-1).Draw(t, "min_hint")
		numSubscribers := rapid.IntRange(2, 6).Draw(
			t, "num_subscribers",
		)
		minHintIndex := rapid.IntRange(1, numSubscribers-1).Draw(
			t, "min_hint_index",
		)

		hints := make([]uint32, numSubscribers)
		numConfs := make([]uint32, numSubscribers)
		for i := range hints {
			if i == minHintIndex {
				hints[i] = minHint
			} else {
				hints[i] = rapid.Uint32Range(
					minHint+1, tipHeight,
				).Draw(t, "height_hint_"+string(rune('a'+i)))
			}

			numConfs[i] = rapid.Uint32Range(1, 4).Draw(
				t, "num_confs_"+string(rune('a'+i)),
			)
		}

		return confirmationLifecycleScenario{
			tipHeight: tipHeight,
			hints:     hints,
			numConfs:  numConfs,
			cancelIndex: rapid.IntRange(1, numSubscribers-1).Draw(
				t, "cancel_index",
			),
			cancelAfterConf: rapid.Bool().Draw(
				t, "cancel_after_confirmation",
			),
			scanInterruptions: rapid.IntRange(0, 2).Draw(
				t, "scan_interruptions",
			),
			preMineBlocks: rapid.Uint32Range(0, 2).Draw(
				t, "pre_mine_blocks",
			),
			reincludeDelay: rapid.Uint32Range(0, 2).Draw(
				t, "reinclude_delay",
			),
		}
	})
}

// TestConfirmationLifecycleProperty asserts that restart, cancellation, tip
// movement, and reorgs preserve the confirmation contract for every active
// subscriber. Historical scan errors are not passed to TxNotifier. At this
// boundary, an interrupted scan is a dispatch that never calls
// UpdateConfDetails, followed by a restart and re-registration.
func TestConfirmationLifecycleProperty(t *testing.T) {
	program := sha256.Sum256([]byte("confirmation-lifecycle-property"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	txid := tx.TxHash()
	txBlock := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})

	rapid.Check(t, func(t *rapid.T) {
		scenario := genLifecycleScenarios().Draw(t, "scenario")
		cache := newMockHintCache()
		active := make([]bool, len(scenario.hints))
		for i := range active {
			active[i] = true
		}

		var (
			n      *chainntnfs.TxNotifier
			events = make(
				[]*chainntnfs.ConfirmationEvent, len(active),
			)
		)
		numAttempts := scenario.scanInterruptions + 1
		for attempt := range numAttempts {
			n = chainntnfs.NewTxNotifier(
				scenario.tipHeight, chainntnfs.ReorgSafetyLimit,
				cache, cache,
			)

			var dispatches []*chainntnfs.HistoricalConfDispatch
			for i := range active {
				if !active[i] {
					continue
				}

				registration, err := n.RegisterConf(
					&txid, script, scenario.numConfs[i],
					scenario.hints[i],
				)
				require.NoError(t, err)
				events[i] = registration.Event
				if registration.HistoricalDispatch != nil {
					dispatches = append(
						dispatches,
						registration.HistoricalDispatch,
					)
				}
			}

			if attempt == 0 && !scenario.cancelAfterConf {
				events[scenario.cancelIndex].Cancel()
				active[scenario.cancelIndex] = false
				assertConfirmationClosed(
					t, events[scenario.cancelIndex],
				)
			}

			require.NotEmpty(t, dispatches)
			if attempt < scenario.scanInterruptions {
				// Failed scans do not call back. The hint must
				// preserve the obligation across restart.
				n.TearDown()
				for i := range active {
					if active[i] {
						assertConfirmationClosed(
							t, events[i],
						)
					}
				}

				continue
			}

			completionOrder := rapid.Permutation(dispatches).Draw(
				t, "completion_order",
			)
			for _, dispatch := range completionOrder {
				require.NoError(t, n.UpdateConfDetails(
					dispatch.ConfRequest, nil,
				))
			}
		}
		defer n.TearDown()

		currentHeight := scenario.tipHeight
		for range scenario.preMineBlocks {
			currentHeight++
			require.NoError(t, n.ConnectTip(
				emptyBlock, currentHeight,
			))
			require.NoError(t, n.NotifyHeight(currentHeight))
		}

		txHeight := currentHeight + 1
		require.NoError(t, n.ConnectTip(txBlock, txHeight))
		require.NoError(t, n.NotifyHeight(txHeight))
		currentHeight = txHeight

		var maxNumConfs uint32
		for i := range active {
			if active[i] && scenario.numConfs[i] > maxNumConfs {
				maxNumConfs = scenario.numConfs[i]
			}
		}
		for currentHeight < txHeight+maxNumConfs-1 {
			currentHeight++
			require.NoError(t, n.ConnectTip(
				emptyBlock, currentHeight,
			))
			require.NoError(t, n.NotifyHeight(currentHeight))
		}

		for i := range active {
			if !active[i] {
				continue
			}

			assertConfirmation(t, events[i], txHeight)
			assertNoConfirmation(t, events[i])
		}

		if scenario.cancelAfterConf {
			events[scenario.cancelIndex].Cancel()
			active[scenario.cancelIndex] = false
			assertConfirmationClosed(
				t, events[scenario.cancelIndex],
			)
		}

		for currentHeight >= txHeight {
			require.NoError(t, n.DisconnectTip(currentHeight))
			currentHeight--
		}
		for i := range active {
			if !active[i] {
				continue
			}

			select {
			case depth := <-events[i].NegativeConf:
				require.Greater(t, depth, int32(0))
			default:
				t.Fatal("subscriber missed reorg")
			}
			assertNoConfirmation(t, events[i])
		}

		for range scenario.reincludeDelay {
			currentHeight++
			require.NoError(t, n.ConnectTip(
				emptyBlock, currentHeight,
			))
			require.NoError(t, n.NotifyHeight(currentHeight))
		}

		txHeight = currentHeight + 1
		require.NoError(t, n.ConnectTip(txBlock, txHeight))
		require.NoError(t, n.NotifyHeight(txHeight))
		currentHeight = txHeight
		for currentHeight < txHeight+maxNumConfs-1 {
			currentHeight++
			require.NoError(t, n.ConnectTip(
				emptyBlock, currentHeight,
			))
			require.NoError(t, n.NotifyHeight(currentHeight))
		}

		for i := range active {
			if !active[i] {
				continue
			}

			assertConfirmation(t, events[i], txHeight)
			assertNoConfirmation(t, events[i])
		}
	})
}

func assertConfirmation(t rapid.TB, event *chainntnfs.ConfirmationEvent,
	height uint32) {

	t.Helper()
	select {
	case details := <-event.Confirmed:
		require.Equal(t, height, details.BlockHeight)
	default:
		t.Fatal("subscriber missed confirmation")
	}
}

func assertNoConfirmation(t rapid.TB, event *chainntnfs.ConfirmationEvent) {
	t.Helper()
	select {
	case <-event.Confirmed:
		t.Fatal("subscriber received duplicate confirmation")
	default:
	}
}

func assertConfirmationClosed(t rapid.TB,
	event *chainntnfs.ConfirmationEvent) {

	t.Helper()
	select {
	case _, ok := <-event.Confirmed:
		require.False(t, ok)
	default:
		t.Fatal("expected closed confirmation channel")
	}
}

// TestConfirmationEqualHeightHintsShareScan ensures a cached hint can raise a
// shared scan boundary without causing an identical subscriber hint to rescan
// the range below the cache.
func TestConfirmationEqualHeightHintsShareScan(t *testing.T) {
	const (
		heightHint   = uint32(50)
		cachedHeight = uint32(100)
		tipHeight    = uint32(103)
	)

	program := sha256.Sum256([]byte("confirmation-equal-height-hints"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	txid := chainntnfs.ZeroHash

	cache := newMockHintCache()
	confRequest, err := chainntnfs.NewConfRequest(&txid, script)
	require.NoError(t, err)
	require.NoError(t, cache.CommitConfirmHint(cachedHeight, confRequest))

	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	first, err := n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	require.Equal(t, cachedHeight, first.HistoricalDispatch.StartHeight)
	require.Equal(t, tipHeight, first.HistoricalDispatch.EndHeight)

	second, err := n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
}
