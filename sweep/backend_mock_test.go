package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type mockBackend struct {
	// TODO: lock

	notifier *MockNotifier

	confirmedSpendInputs map[wire.OutPoint]struct{}

	unconfirmedTxes        map[chainhash.Hash]*wire.MsgTx
	unconfirmedSpendInputs map[wire.OutPoint]struct{}
}

func newMockBackend(notifier *MockNotifier) *mockBackend {
	return &mockBackend{
		notifier:               notifier,
		unconfirmedTxes:        make(map[chainhash.Hash]*wire.MsgTx),
		confirmedSpendInputs:   make(map[wire.OutPoint]struct{}),
		unconfirmedSpendInputs: make(map[wire.OutPoint]struct{}),
	}
}

func (b *mockBackend) publishTransaction(tx *wire.MsgTx) error {

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

func (b *mockBackend) deleteUnconfirmed(txHash chainhash.Hash) {
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
