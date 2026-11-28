package chainntnfs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// legacyHintCache implements the original height-only cache interfaces but not
// their origin-aware extensions.
type legacyHintCache struct {
	chainntnfs.ConfirmHintCache
	chainntnfs.SpendHintCache
}

var _ chainntnfs.ConfirmHintCache = (*legacyHintCache)(nil)
var _ chainntnfs.SpendHintCache = (*legacyHintCache)(nil)

// TestTxNotifierLegacyHintCacheCompatibility asserts that the original
// exported cache interfaces remain usable. Their height-only hints are not
// used to skip blocks because they cannot prove the origin of their coverage.
func TestTxNotifierLegacyHintCacheCompatibility(t *testing.T) {
	const (
		tipHeight    = uint32(200)
		cachedHeight = uint32(180)
		heightHint   = uint32(100)
	)

	cache := newMockHintCache()
	legacy := &legacyHintCache{
		ConfirmHintCache: cache,
		SpendHintCache:   cache,
	}
	script, tx := staleHintTx("legacy-cache-compatibility")
	txid := tx.TxHash()
	confRequest, err := chainntnfs.NewConfRequest(&txid, script)
	require.NoError(t, err)
	require.NoError(t, legacy.CommitConfirmHint(
		cachedHeight, confRequest,
	))

	outpoint, spendScript, _ := staleHintSpend("legacy-cache-compatibility")
	spendRequest, err := chainntnfs.NewSpendRequest(&outpoint, spendScript)
	require.NoError(t, err)
	require.NoError(t, legacy.CommitSpendHint(
		cachedHeight, spendRequest,
	))

	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, legacy, legacy,
	)
	t.Cleanup(n.TearDown)

	conf, err := n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NotNil(t, conf.HistoricalDispatch)
	require.Equal(t, heightHint, conf.HistoricalDispatch.StartHeight)

	spend, err := n.RegisterSpend(&outpoint, spendScript, heightHint)
	require.NoError(t, err)
	require.NotNil(t, spend.HistoricalDispatch)
	require.Equal(t, heightHint, spend.HistoricalDispatch.StartHeight)
}

// TestTxNotifierHintOriginGuardsEarlierHint asserts that a hint persisted while
// only a late-hint subscriber watched a request cannot hide an earlier
// confirmation or spend from the next subscriber after a restart. The first
// subscriber's hint is later than the real event, so its scans never covered
// that block. A subscriber whose hint precedes the cached hint's origin must
// therefore scan from its own hint, even when it registers first.
func TestTxNotifierHintOriginGuardsEarlierHint(t *testing.T) {
	const (
		startHeight = uint32(1000)
		tipHeight   = uint32(1010)
		lateHint    = uint32(990)
		earlyHint   = uint32(850)
	)

	t.Run("confirmation", func(t *testing.T) {
		script, tx := staleHintTx("hint-origin-conf")
		txid := tx.TxHash()
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		// The late subscriber's scan and tip tracking leave a hint at
		// the tip with the late hint as its origin.
		late, err := n.RegisterConf(&txid, script, 1, lateHint)
		require.NoError(t, err)
		request := late.HistoricalDispatch.ConfRequest
		require.NoError(t, n.UpdateConfDetails(request, nil))
		advanceEmptyChain(t, n, startHeight+1, tipHeight)
		hint, err := cache.QueryConfirmHintWithOrigin(request)
		require.NoError(t, err)
		require.Equal(t, chainntnfs.HeightHint{
			Height: tipHeight, Origin: lateHint,
		}, hint)

		// After a restart, the earlier subscriber registers first and
		// cannot use a hint whose origin is above its own hint.
		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterConf(&txid, script, 1, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})

	t.Run("spend", func(t *testing.T) {
		outpoint, script, _ := staleHintSpend("hint-origin-spend")
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		late, err := n.RegisterSpend(&outpoint, script, lateHint)
		require.NoError(t, err)
		request := late.HistoricalDispatch.SpendRequest
		require.NoError(t, n.UpdateSpendDetails(request, nil))
		advanceEmptyChain(t, n, startHeight+1, tipHeight)
		hint, err := cache.QuerySpendHintWithOrigin(request)
		require.NoError(t, err)
		require.Equal(t, chainntnfs.HeightHint{
			Height: tipHeight, Origin: lateHint,
		}, hint)

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterSpend(&outpoint, script, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})

	// Progress persisted by a backend while the late scan is outstanding
	// carries the scan's origin too.
	t.Run("spend progress", func(t *testing.T) {
		outpoint, script, _ := staleHintSpend("hint-origin-progress")
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		late, err := n.RegisterSpend(&outpoint, script, lateHint)
		require.NoError(t, err)
		require.NotNil(t, late.HistoricalDispatch)
		require.NoError(t, cache.CommitSpendHints(
			late.HistoricalDispatch.ProgressHints(startHeight),
		))

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterSpend(&outpoint, script, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})
}

// TestTxNotifierHintOriginPrefixScans asserts how prefix scans use hint
// origins. A cached hint is resumed from by a subscriber at or above its
// origin, and a later subscriber whose hint is inside the range it covers
// shares the existing scan, while one below the origin scans only the prefix
// below it. A legacy hint only covers down to its first subscriber's hint,
// and a known confirmation is delivered without any prefix scan. A set whose
// only subscriber had a late hint and canceled still extends its coverage for
// an earlier subscriber.
func TestTxNotifierHintOriginPrefixScans(t *testing.T) {
	const (
		tipHeight = uint32(200)
		origin    = uint32(100)
		cached    = uint32(180)
	)

	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	script, tx := staleHintTx("hint-origin-prefix")
	txid := tx.TxHash()
	confRequest, err := chainntnfs.NewConfRequest(&txid, script)
	require.NoError(t, err)
	require.NoError(t, cache.CommitConfirmHints(
		chainntnfs.ConfirmHints{
			confRequest: {Height: cached, Origin: origin},
		},
	))

	// The first subscriber resumes from the cached hint.
	first, err := n.RegisterConf(&txid, script, 1, origin+50)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	require.Equal(t, cached, first.HistoricalDispatch.StartHeight)

	// A subscriber inside the cached range shares that scan.
	second, err := n.RegisterConf(&txid, script, 1, origin)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)

	// A subscriber below the origin scans only the prefix below it.
	third, err := n.RegisterConf(&txid, script, 1, origin-10)
	require.NoError(t, err)
	prefix := third.HistoricalDispatch
	require.NotNil(t, prefix)
	require.True(t, prefix.Supplemental)
	require.Equal(t, origin-10, prefix.StartHeight)
	require.Equal(t, origin-1, prefix.EndHeight)

	// A legacy hint without an origin is trusted by the first subscriber,
	// and only as far down as that subscriber's own hint, so an earlier
	// subscriber still scans the prefix below it.
	legacyScript, legacyTx := staleHintTx("hint-origin-legacy")
	legacyTxid := legacyTx.TxHash()
	legacyRequest, err := chainntnfs.NewConfRequest(
		&legacyTxid, legacyScript,
	)
	require.NoError(t, err)
	require.NoError(t, cache.commitConfirmHeight(cached, legacyRequest))
	legacy, err := n.RegisterConf(&legacyTxid, legacyScript, 1, origin)
	require.NoError(t, err)
	require.NotNil(t, legacy.HistoricalDispatch)
	require.Equal(t, cached, legacy.HistoricalDispatch.StartHeight)
	legacyEarly, err := n.RegisterConf(
		&legacyTxid, legacyScript, 1, origin-10,
	)
	require.NoError(t, err)
	require.NotNil(t, legacyEarly.HistoricalDispatch)
	require.Equal(t, origin-1, legacyEarly.HistoricalDispatch.EndHeight)

	// Once the confirmation is known, a subscriber with an earlier hint
	// is notified right away instead of waiting on a prefix scan.
	knownScript, knownTx := staleHintTx("hint-origin-known")
	knownTxid := knownTx.TxHash()
	known, err := n.RegisterConf(&knownTxid, knownScript, 1, cached)
	require.NoError(t, err)
	require.NotNil(t, known.HistoricalDispatch)
	require.NoError(t, n.UpdateConfDetails(
		known.HistoricalDispatch.ConfRequest,
		&chainntnfs.TxConfirmation{
			BlockHeight: cached + 5,
			Tx:          knownTx,
		},
	))
	knownEarly, err := n.RegisterConf(&knownTxid, knownScript, 1, origin)
	require.NoError(t, err)
	require.Nil(t, knownEarly.HistoricalDispatch)
	select {
	case details := <-knownEarly.Event.Confirmed:
		require.Equal(t, cached+5, details.BlockHeight)
	default:
		require.FailNow(
			t, "earlier subscriber missed known confirmation",
		)
	}

	// A subscriber whose hint is above the tip needs no scan. A later,
	// earlier subscriber's prefix then ends at the tip, since a backend
	// can't scan blocks it doesn't have yet, and later blocks are seen at
	// tip anyway.
	aboveScript, aboveTx := staleHintTx("hint-origin-above-tip")
	aboveTxid := aboveTx.TxHash()
	above, err := n.RegisterConf(&aboveTxid, aboveScript, 1, tipHeight+5)
	require.NoError(t, err)
	require.Nil(t, above.HistoricalDispatch)
	belowConf, err := n.RegisterConf(&aboveTxid, aboveScript, 1, origin)
	require.NoError(t, err)
	require.NotNil(t, belowConf.HistoricalDispatch)
	require.Equal(t, tipHeight, belowConf.HistoricalDispatch.EndHeight)

	aboveOutpoint, aboveSpendScript, _ := staleHintSpend(
		"hint-origin-above-tip",
	)
	aboveSpend, err := n.RegisterSpend(
		&aboveOutpoint, aboveSpendScript, tipHeight+5,
	)
	require.NoError(t, err)
	require.Nil(t, aboveSpend.HistoricalDispatch)
	belowSpend, err := n.RegisterSpend(
		&aboveOutpoint, aboveSpendScript, origin,
	)
	require.NoError(t, err)
	require.NotNil(t, belowSpend.HistoricalDispatch)
	require.Equal(t, tipHeight, belowSpend.HistoricalDispatch.EndHeight)

	// A late subscriber cancels before an earlier one arrives. The set
	// still records the late coverage, so the earlier subscriber scans the
	// prefix below it.
	lateScript, lateTx := staleHintTx("hint-origin-canceled")
	lateTxid := lateTx.TxHash()
	late, err := n.RegisterConf(&lateTxid, lateScript, 1, 190)
	require.NoError(t, err)
	require.NotNil(t, late.HistoricalDispatch)
	require.NoError(t, n.UpdateConfDetails(
		late.HistoricalDispatch.ConfRequest, nil,
	))
	late.Event.Cancel()

	early, err := n.RegisterConf(&lateTxid, lateScript, 1, 150)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)
	require.Equal(t, uint32(150), early.HistoricalDispatch.StartHeight)
	require.Equal(t, uint32(189), early.HistoricalDispatch.EndHeight)
}

// TestTxNotifierPendingScanAfterDeepReorg asserts that a prefix which lies
// entirely above a reorged tip does not mark a set complete while its original
// historical scan remains outstanding. A pending set must not advance its
// cached hint when the next block connects.
func TestTxNotifierPendingScanAfterDeepReorg(t *testing.T) {
	const (
		startHeight = uint32(100)
		reorgTip    = uint32(50)
		earlierHint = uint32(80)
	)

	disconnect := func(t *testing.T, n *chainntnfs.TxNotifier) {
		t.Helper()
		for height := startHeight; height > reorgTip; height-- {
			require.NoError(t, n.DisconnectTip(height))
		}
	}

	t.Run("confirmation", func(t *testing.T) {
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)

		script, tx := staleHintTx("pending-deep-reorg-conf")
		txid := tx.TxHash()
		initial, err := n.RegisterConf(
			&txid, script, 1, startHeight,
		)
		require.NoError(t, err)
		require.NotNil(t, initial.HistoricalDispatch)
		request := initial.HistoricalDispatch.ConfRequest

		disconnect(t, n)
		earlier, err := n.RegisterConf(&txid, script, 1, earlierHint)
		require.NoError(t, err)
		require.Nil(t, earlier.HistoricalDispatch)

		block := btcutil.NewBlock(&wire.MsgBlock{})
		require.NoError(t, n.ConnectTip(block, reorgTip+1))
		require.NoError(t, n.NotifyHeight(reorgTip+1))
		_, err = cache.QueryConfirmHintWithOrigin(request)
		require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	})

	t.Run("spend", func(t *testing.T) {
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)

		outpoint, script, _ := staleHintSpend(
			"pending-deep-reorg-spend",
		)
		initial, err := n.RegisterSpend(
			&outpoint, script, startHeight,
		)
		require.NoError(t, err)
		require.NotNil(t, initial.HistoricalDispatch)
		request := initial.HistoricalDispatch.SpendRequest

		disconnect(t, n)
		earlier, err := n.RegisterSpend(
			&outpoint, script, earlierHint,
		)
		require.NoError(t, err)
		require.Nil(t, earlier.HistoricalDispatch)

		block := btcutil.NewBlock(&wire.MsgBlock{})
		require.NoError(t, n.ConnectTip(block, reorgTip+1))
		require.NoError(t, n.NotifyHeight(reorgTip+1))
		_, err = cache.QuerySpendHintWithOrigin(request)
		require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
	})
}

// staleHintTx returns a transaction with a single output to a script unique to
// the tag, along with that script.
func staleHintTx(tag string) ([]byte, *wire.MsgTx) {
	program := sha256.Sum256([]byte(tag))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

	return script, tx
}

// staleHintSpend returns an outpoint unique to the tag, the script used to
// watch it, and a transaction with a witness that spends it.
func staleHintSpend(tag string) (wire.OutPoint, []byte, *wire.MsgTx) {
	hash := sha256.Sum256([]byte(tag))
	outpoint := wire.OutPoint{Index: 1}
	copy(outpoint.Hash[:], hash[:])
	script := chainntnfs.ZeroTaprootPkScript.Script()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
		Witness:          wire.TxWitness{{0x01}},
	})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

	return outpoint, script, tx
}

// assertNoSpend asserts that no spend notification is waiting on the event.
func assertNoSpend(t *testing.T, event *chainntnfs.SpendEvent) {
	t.Helper()
	select {
	case <-event.Spend:
		require.FailNow(t, "unexpected spend")
	default:
	}
}

// advanceEmptyChain connects and notifies empty blocks at every height from
// start to end, inclusive.
func advanceEmptyChain(t *testing.T, n *chainntnfs.TxNotifier, start,
	end uint32) {

	t.Helper()
	for height := start; height <= end; height++ {
		require.NoError(t, n.ConnectTip(
			btcutil.NewBlock(&wire.MsgBlock{}), height,
		))
		require.NoError(t, n.NotifyHeight(height))
	}
}

// TestTxNotifierScriptMatchPendingPrefix checks restart coverage when a
// newer payment to a watched script disconnects during an earlier scan.
func TestTxNotifierScriptMatchPendingPrefix(t *testing.T) {
	// Arrange two distinct payments to the same script. Only the older
	// payment will remain on chain after the tip disconnects.
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		74, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	script, older := staleHintTx("pending-script-prefix")
	// The prior chain had been scanned to 90. This record must not hide
	// an earlier payment introduced by the replacement chain.
	request, err := chainntnfs.NewConfRequest(nil, script)
	require.NoError(t, err)
	require.NoError(t, cache.CommitConfirmHints(chainntnfs.ConfirmHints{
		request: {Height: 90, Origin: 50},
	}))

	newer := older.Copy()
	newer.TxOut[0].Value++
	at75 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{older},
	})
	at100 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{newer},
	})
	require.NoError(t, n.ConnectTip(at75, 75))
	require.NoError(t, n.NotifyHeight(75))
	advanceEmptyChain(t, n, 76, 99)
	require.NoError(t, n.ConnectTip(at100, 100))
	require.NoError(t, n.NotifyHeight(100))

	// Register by script after both payments so the newer payment is in
	// the initial scan and the older one is in the outstanding prefix.
	late, err := n.RegisterConf(nil, script, 1, 90)
	require.NoError(t, err)
	require.NotNil(t, late.HistoricalDispatch)
	early, err := n.RegisterConf(nil, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)

	// Act by reporting only the later match. Delivery must be possible
	// immediately, even though the prefix callback hasn't arrived.
	err = n.UpdateConfDetails(request, &chainntnfs.TxConfirmation{
		BlockHash:   at100.Hash(),
		BlockHeight: 100,
		Tx:          newer,
	})
	require.NoError(t, err)
	select {
	case details := <-early.Event.Confirmed:
		require.Equal(t, newer.TxHash(), details.Tx.TxHash())
	default:
		t.Fatal("missing confirmation while prefix is pending")
	}

	// Observe the hint at the match, at its disconnect, and after an
	// empty replacement connects. The last observation exercises ordinary
	// tip updates once the notifier has cleared the cached match.
	_, matchErr := cache.QueryConfirmHintWithOrigin(request)
	require.ErrorIs(t, matchErr, chainntnfs.ErrConfirmHintNotFound)
	require.NoError(t, n.DisconnectTip(100))
	_, reorgErr := cache.QueryConfirmHintWithOrigin(request)
	advanceEmptyChain(t, n, 100, 100)
	_, tipErr := cache.QueryConfirmHintWithOrigin(request)
	require.NoError(t, n.DisconnectTip(100))
	n.TearDown()

	// Assert that the replacement notifier schedules the surviving
	// payment's block and that none of the three updates certified the
	// outstanding prefix after discarding the old chain's hint.
	n = chainntnfs.NewTxNotifier(
		99, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	restarted, err := n.RegisterConf(nil, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, restarted.HistoricalDispatch)
	require.EqualValues(t, 50, restarted.HistoricalDispatch.StartHeight)
	require.GreaterOrEqual(
		t, restarted.HistoricalDispatch.EndHeight, uint32(75),
	)
	require.ErrorIs(t, reorgErr, chainntnfs.ErrConfirmHintNotFound)
	require.ErrorIs(t, tipErr, chainntnfs.ErrConfirmHintNotFound)
}

// TestTxNotifierScriptMatchAfterRollback checks that restarting after a tip
// match can recover a payment below the previous chain's cached scan height.
func TestTxNotifierScriptMatchAfterRollback(t *testing.T) {
	// Arrange a hint verified on the original chain and two outstanding
	// scans, so a replacement payment can arrive before either callback.
	cache := newMockHintCache()
	script, payment := staleHintTx("replacement-payment")
	request, err := chainntnfs.NewConfRequest(nil, script)
	require.NoError(t, err)
	require.NoError(t, cache.CommitConfirmHints(chainntnfs.ConfirmHints{
		request: {Height: 100, Origin: 50},
	}))
	n := chainntnfs.NewTxNotifier(
		100, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	_, err = n.RegisterConf(nil, script, 1, 100)
	require.NoError(t, err)
	early, err := n.RegisterConf(nil, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)

	// Act by replacing the last two blocks. The notifier sees the payment
	// at tip below the cached height, then restarts at the original height.
	require.NoError(t, n.DisconnectTip(100))
	require.NoError(t, n.DisconnectTip(99))
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{payment},
	})
	require.NoError(t, n.ConnectTip(block, 99))
	require.NoError(t, n.NotifyHeight(99))
	advanceEmptyChain(t, n, 100, 100)
	n.TearDown()

	// Assert that the next subscriber's actual scan includes the
	// replacement payment, even after the notifier has lost its details.
	n = chainntnfs.NewTxNotifier(
		100, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	restarted, err := n.RegisterConf(nil, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, restarted.HistoricalDispatch)
	require.LessOrEqual(
		t, restarted.HistoricalDispatch.StartHeight, uint32(99),
	)
	require.GreaterOrEqual(
		t, restarted.HistoricalDispatch.EndHeight, uint32(99),
	)
}

// TestTxNotifierScriptPrefixCompletesBeforeReorg checks that a prefix match
// reported while a newer payment is active remains discoverable on restart.
func TestTxNotifierScriptPrefixCompletesBeforeReorg(t *testing.T) {
	// Arrange overlapping subscriptions for two payments to one script.
	// The first scan covers the newer payment, the prefix covers the older.
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		100, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	script, older := staleHintTx("completed-script-prefix")
	newer := older.Copy()
	newer.TxOut[0].Value++
	_, err := n.RegisterConf(nil, script, 1, 90)
	require.NoError(t, err)
	early, err := n.RegisterConf(nil, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)
	request := early.HistoricalDispatch.ConfRequest

	// Act by completing both scans before disconnecting the active
	// payment. Keep the hint cache across the notifier restart.
	err = n.UpdateConfDetails(request, &chainntnfs.TxConfirmation{
		Tx:          newer,
		BlockHeight: 100,
	})
	require.NoError(t, err)
	err = n.UpdateConfDetails(request, &chainntnfs.TxConfirmation{
		Tx:          older,
		BlockHeight: 75,
	})
	require.NoError(t, err)
	require.NoError(t, n.DisconnectTip(100))
	n.TearDown()

	// Assert that even a subscriber starting exactly at the surviving
	// payment's height receives a scan that includes its block.
	n = chainntnfs.NewTxNotifier(
		99, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	restarted, err := n.RegisterConf(nil, script, 1, 75)
	require.NoError(t, err)
	require.NotNil(t, restarted.HistoricalDispatch)
	require.EqualValues(t, 75, restarted.HistoricalDispatch.StartHeight)
	require.GreaterOrEqual(
		t, restarted.HistoricalDispatch.EndHeight, uint32(75),
	)
}
