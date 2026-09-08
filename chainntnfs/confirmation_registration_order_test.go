package chainntnfs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
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
				testCase.prefixFirst, testCase.restartBeforePrefix,
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

	// The earlier range must be durable as soon as it is accepted. If the
	// notifier restarts before the prefix completes, the cache must cause the
	// replacement subscription to scan that range again.
	switch {
	case restartBeforePrefix:
		hint, err := cache.QueryConfirmHint(
			early.HistoricalDispatch.ConfRequest,
		)
		require.NoError(t, err)
		require.Equal(t, minedHeight, hint)

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			restartHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
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

	hint, err := cache.QueryConfirmHint(early.HistoricalDispatch.ConfRequest)
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
		t.Fatal("early subscriber did not receive historical confirmation")
	}

	select {
	case details := <-late.Event.Confirmed:
		require.Equal(t, minedHeight, details.BlockHeight)
	default:
		t.Fatal("late subscriber did not receive historical confirmation")
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
