package lnwallet

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
)

type mockRebroadcaster struct {
	started atomic.Bool

	rebroadcastAttempt chan struct{}

	falseStart bool

	confSignal chan struct{}

	startSignal chan struct{}
}

func newMockRebroadcaster(falseStart bool) *mockRebroadcaster {
	return &mockRebroadcaster{
		rebroadcastAttempt: make(chan struct{}, 1),
		falseStart:         falseStart,
		confSignal:         make(chan struct{}, 1),
		startSignal:        make(chan struct{}),
	}
}

func (m *mockRebroadcaster) Start() error {
	if !m.falseStart {
		defer m.started.Store(true)
		defer close(m.startSignal)
	}

	return nil
}

func (m *mockRebroadcaster) Started() bool {
	return m.started.Load()
}

func (m *mockRebroadcaster) Stop() {
}

// Broadcast enqueues a transaction to be rebroadcast until it's been
// confirmed.
func (m *mockRebroadcaster) Broadcast(tx *wire.MsgTx) error {
	m.rebroadcastAttempt <- struct{}{}
	return nil
}

func (m *mockRebroadcaster) MarkAsConfirmed(txid chainhash.Hash) {
	m.confSignal <- struct{}{}
}

func assertBroadcasterBypass(t *testing.T, wallet *LightningWallet,
	rebroadcaster *mockRebroadcaster,
	walletController *mockWalletController) {

	testTx := wire.NewMsgTx(2)
	timeout := time.Second * 1

	require.NoError(t, wallet.PublishTransaction(testTx, ""))

	// The tx should go to the backend.
	_, err := lnutils.RecvOrTimeout(
		walletController.PublishedTransactions, timeout,
	)
	require.NoError(t, err)

	// It shouldn't go to the rebroadcaster.
	select {
	case <-time.After(timeout):
	case <-rebroadcaster.rebroadcastAttempt:
		t.Fatal("tx sent to rebroadcaster")
	}
}

func assertBroadcasterSend(t *testing.T, wallet *LightningWallet,
	rebroadcaster *mockRebroadcaster,
	walletController *mockWalletController) {

	testTx := wire.NewMsgTx(2)
	testTx.AddTxOut(&wire.TxOut{})

	timeout := time.Second * 1

	require.NoError(t, wallet.PublishTransaction(testTx, ""))

	// The tx should go to the backend.
	_, err := lnutils.RecvOrTimeout(
		walletController.PublishedTransactions, timeout,
	)
	require.NoError(t, err)

	// It should also go to the rebroadcaster.
	select {
	case <-time.After(timeout):
		t.Fatal("tx not sent to rebroadcaster")
	case <-rebroadcaster.rebroadcastAttempt:
	}
}

// TestWalletRebroadcaster tests that the wallet properly manages the existence
// or lack of existence of the rebroadcaster, and also properly marks the
// transaction as confirmed.
func TestWalletRebroadcaster(t *testing.T) {
	t.Parallel()

	rebroadcaster := newMockRebroadcaster(false)
	walletController := &mockWalletController{
		PublishedTransactions: make(chan *wire.MsgTx, 1),
	}
	chainIO := &mockChainIO{}
	notifier := &mockChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
		ConfChan:  make(chan *chainntnfs.TxConfirmation, 1),
	}
	cfg := &Config{
		Rebroadcaster:    rebroadcaster,
		WalletController: walletController,
		Notifier:         notifier,
		ChainIO:          chainIO,
	}

	t.Run("rebroadcast bypass", func(t *testing.T) {
		// We'll make a copy of the config, but without the
		// broadcaster.
		testCfg := *cfg
		testCfg.Rebroadcaster = nil

		wallet, err := NewLightningWallet(testCfg)
		require.NoError(t, err)
		require.NoError(t, wallet.Startup())

		// If we try to broadcast, it should go straight to the wallet
		// backend and skip the broadcaster.
		assertBroadcasterBypass(
			t, wallet, rebroadcaster, walletController,
		)

		err = wallet.Shutdown()
		require.NoError(t, err)

		// If we make a new wallet, that has the broadcaster, but
		// hasn't started yet, we should see the same behavior.
		testCfg.Rebroadcaster = newMockRebroadcaster(true)

		wallet, err = NewLightningWallet(testCfg)
		require.NoError(t, err)
		require.NoError(t, wallet.Startup())

		assertBroadcasterBypass(
			t, wallet, rebroadcaster, walletController,
		)

		err = wallet.Shutdown()
		require.NoError(t, err)
	})

	t.Run("rebroadcast normal", func(t *testing.T) {
		wallet, err := NewLightningWallet(*cfg)
		require.NoError(t, err)
		require.NoError(t, wallet.Startup())

		defer func() {
			err = wallet.Shutdown()
			require.NoError(t, err)
		}()

		// Wait for the broadcaster to start.
		_, err = lnutils.RecvOrTimeout(
			rebroadcaster.startSignal, time.Second,
		)
		require.NoError(t, err)

		// We'll now broadcast a new test transaction, asserting that
		// it goes to both the backend and the rebroadcaster.
		assertBroadcasterSend(
			t, wallet, rebroadcaster, walletController,
		)

		// We'll now mark the transaction as confirmed, and assert that
		// the rebroadcaster was notified.
		notifier.ConfChan <- &chainntnfs.TxConfirmation{}

		_, err = lnutils.RecvOrTimeout(
			rebroadcaster.confSignal, time.Second,
		)
		require.NoError(t, err)
	})
}
