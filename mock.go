package lnd

import (
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// The block height returned by the mock BlockChainIO's GetBestBlock.
const fundingBroadcastHeight = 123

type mockNotfier struct {
	confChannel chan *chainntnfs.TxConfirmation
}

func (m *mockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChannel,
	}, nil
}
func (m *mockNotfier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

func (m *mockNotfier) Start() error {
	return nil
}

func (m *mockNotfier) Started() bool {
	return true
}

func (m *mockNotfier) Stop() error {
	return nil
}
func (m *mockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

// mockSpendNotifier extends the mockNotifier so that spend notifications can be
// triggered and delivered to subscribers.
type mockSpendNotifier struct {
	*mockNotfier
	spendMap map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	spends   map[wire.OutPoint]*chainntnfs.SpendDetail
	mtx      sync.Mutex
}

func makeMockSpendNotifier() *mockSpendNotifier {
	return &mockSpendNotifier{
		mockNotfier: &mockNotfier{
			confChannel: make(chan *chainntnfs.TxConfirmation),
		},
		spendMap: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
		spends:   make(map[wire.OutPoint]*chainntnfs.SpendDetail),
	}
}

func (m *mockSpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	if detail, ok := m.spends[*outpoint]; ok {
		// Deliver spend immediately if details are already known.
		spendChan <- &chainntnfs.SpendDetail{
			SpentOutPoint:     detail.SpentOutPoint,
			SpendingHeight:    detail.SpendingHeight,
			SpendingTx:        detail.SpendingTx,
			SpenderTxHash:     detail.SpenderTxHash,
			SpenderInputIndex: detail.SpenderInputIndex,
		}
	} else {
		// Otherwise, queue the notification for delivery if the spend
		// is ever received.
		m.spendMap[*outpoint] = append(m.spendMap[*outpoint], spendChan)
	}

	return &chainntnfs.SpendEvent{
		Spend: spendChan,
		Cancel: func() {
		},
	}, nil
}

// Spend dispatches SpendDetails to all subscribers of the outpoint. The details
// will include the transaction and height provided by the caller.
func (m *mockSpendNotifier) Spend(outpoint *wire.OutPoint, height int32,
	txn *wire.MsgTx) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	txnHash := txn.TxHash()
	details := &chainntnfs.SpendDetail{
		SpentOutPoint:     outpoint,
		SpendingHeight:    height,
		SpendingTx:        txn,
		SpenderTxHash:     &txnHash,
		SpenderInputIndex: outpoint.Index,
	}

	// Cache details in case of late registration.
	if _, ok := m.spends[*outpoint]; !ok {
		m.spends[*outpoint] = details
	}

	// Deliver any backlogged spend notifications.
	if spendChans, ok := m.spendMap[*outpoint]; ok {
		delete(m.spendMap, *outpoint)
		for _, spendChan := range spendChans {
			spendChan <- &chainntnfs.SpendDetail{
				SpentOutPoint:     details.SpentOutPoint,
				SpendingHeight:    details.SpendingHeight,
				SpendingTx:        details.SpendingTx,
				SpenderTxHash:     details.SpenderTxHash,
				SpenderInputIndex: details.SpenderInputIndex,
			}
		}
	}
}

type mockChainIO struct {
	bestHeight int32
}

var _ lnwallet.BlockChainIO = (*mockChainIO)(nil)

func (m *mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return chaincfg.TestNet3Params.GenesisHash, m.bestHeight, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}
