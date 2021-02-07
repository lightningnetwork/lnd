package sweep

import (
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// mockBackend simulates a chain backend for realistic behaviour in unit tests
// around double spends.
type mockBackend struct {
	t *testing.T

	lock sync.Mutex

	notifier *MockNotifier

	confirmedSpendInputs map[wire.OutPoint]struct{}

	unconfirmedTxes        map[chainhash.Hash]*wire.MsgTx
	unconfirmedSpendInputs map[wire.OutPoint]struct{}

	publishChan chan wire.MsgTx

	walletUtxos []*lnwallet.Utxo
	utxoCnt     int
}

func newMockBackend(t *testing.T, notifier *MockNotifier) *mockBackend {
	return &mockBackend{
		t:                      t,
		notifier:               notifier,
		unconfirmedTxes:        make(map[chainhash.Hash]*wire.MsgTx),
		confirmedSpendInputs:   make(map[wire.OutPoint]struct{}),
		unconfirmedSpendInputs: make(map[wire.OutPoint]struct{}),
		publishChan:            make(chan wire.MsgTx, 2),
	}
}

func (b *mockBackend) publishTransaction(tx *wire.MsgTx) error {
	b.lock.Lock()
	defer b.lock.Unlock()

	txHash := tx.TxHash()
	if _, ok := b.unconfirmedTxes[txHash]; ok {
		// Tx already exists
		testLog.Tracef("mockBackend duplicate tx %v", tx.TxHash())
		return lnwallet.ErrDoubleSpend
	}

	for _, in := range tx.TxIn {
		if _, ok := b.unconfirmedSpendInputs[in.PreviousOutPoint]; ok {
			// Double spend
			testLog.Tracef("mockBackend double spend tx %v", tx.TxHash())
			return lnwallet.ErrDoubleSpend
		}

		if _, ok := b.confirmedSpendInputs[in.PreviousOutPoint]; ok {
			// Already included in block
			testLog.Tracef("mockBackend already in block tx %v", tx.TxHash())
			return lnwallet.ErrDoubleSpend
		}
	}

	b.unconfirmedTxes[txHash] = tx
	for _, in := range tx.TxIn {
		b.unconfirmedSpendInputs[in.PreviousOutPoint] = struct{}{}
	}

	testLog.Tracef("mockBackend publish tx %v", tx.TxHash())

	return nil
}

func (b *mockBackend) PublishTransaction(tx *wire.MsgTx, _ string) error {
	log.Tracef("Publishing tx %v", tx.TxHash())
	err := b.publishTransaction(tx)
	select {
	case b.publishChan <- *tx:
	case <-time.After(defaultTestTimeout):
		b.t.Fatalf("unexpected tx published")
	}
	return err
}

func (b *mockBackend) ListUnspentWitness(minconfirms, maxconfirms int32) (
	[]*lnwallet.Utxo, error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	// Each time we list output, we increment the utxo counter, to
	// ensure we don't return the same outpoint every time.
	b.utxoCnt++

	for i := range b.walletUtxos {
		b.walletUtxos[i].OutPoint.Hash[0] = byte(b.utxoCnt)
	}

	return b.walletUtxos, nil
}

func (b *mockBackend) WithCoinSelectLock(f func() error) error {
	return f()
}

func (b *mockBackend) deleteUnconfirmed(txHash chainhash.Hash) {
	b.lock.Lock()
	defer b.lock.Unlock()

	tx, ok := b.unconfirmedTxes[txHash]
	if !ok {
		// Tx already exists
		testLog.Errorf("mockBackend delete tx not existing %v", txHash)
		return
	}

	testLog.Tracef("mockBackend delete tx %v", tx.TxHash())
	delete(b.unconfirmedTxes, txHash)
	for _, in := range tx.TxIn {
		delete(b.unconfirmedSpendInputs, in.PreviousOutPoint)
	}
}

func (b *mockBackend) mine() {
	b.lock.Lock()
	defer b.lock.Unlock()

	notifications := make(map[wire.OutPoint]*wire.MsgTx)
	for _, tx := range b.unconfirmedTxes {
		testLog.Tracef("mockBackend mining tx %v", tx.TxHash())
		for _, in := range tx.TxIn {
			b.confirmedSpendInputs[in.PreviousOutPoint] = struct{}{}
			notifications[in.PreviousOutPoint] = tx
		}
	}
	b.unconfirmedSpendInputs = make(map[wire.OutPoint]struct{})
	b.unconfirmedTxes = make(map[chainhash.Hash]*wire.MsgTx)

	for outpoint, tx := range notifications {
		testLog.Tracef("mockBackend delivering spend ntfn for %v",
			outpoint)
		b.notifier.SpendOutpoint(outpoint, *tx)
	}
}

func (b *mockBackend) isDone() bool {
	return len(b.unconfirmedTxes) == 0
}
