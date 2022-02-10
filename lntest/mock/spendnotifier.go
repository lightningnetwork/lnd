package mock

import (
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// SpendNotifier extends the mock.ChainNotifier so that spend
// notifications can be triggered and delivered to subscribers.
type SpendNotifier struct {
	*ChainNotifier
	spendMap map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	spends   map[wire.OutPoint]*chainntnfs.SpendDetail
	mtx      sync.Mutex
}

// MakeMockSpendNotifier creates a SpendNotifier.
func MakeMockSpendNotifier() *SpendNotifier {
	return &SpendNotifier{
		ChainNotifier: &ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		spendMap: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
		spends:   make(map[wire.OutPoint]*chainntnfs.SpendDetail),
	}
}

// RegisterSpendNtfn registers a spend notification for a specified outpoint.
func (s *SpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	s.mtx.Lock()
	defer s.mtx.Unlock()

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	if detail, ok := s.spends[*outpoint]; ok {
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
		s.spendMap[*outpoint] = append(s.spendMap[*outpoint], spendChan)
	}

	return &chainntnfs.SpendEvent{
		Spend:  spendChan,
		Cancel: func() {},
	}, nil
}

// Spend dispatches SpendDetails to all subscribers of the outpoint. The details
// will includethe transaction and height provided by the caller.
func (s *SpendNotifier) Spend(outpoint *wire.OutPoint, height int32,
	txn *wire.MsgTx) {

	s.mtx.Lock()
	defer s.mtx.Unlock()

	var inputIndex uint32
	for i, in := range txn.TxIn {
		if in.PreviousOutPoint == *outpoint {
			inputIndex = uint32(i)
		}
	}

	txnHash := txn.TxHash()
	details := &chainntnfs.SpendDetail{
		SpentOutPoint:     outpoint,
		SpendingHeight:    height,
		SpendingTx:        txn,
		SpenderTxHash:     &txnHash,
		SpenderInputIndex: inputIndex,
	}

	// Cache details in case of late registration.
	if _, ok := s.spends[*outpoint]; !ok {
		s.spends[*outpoint] = details
	}

	// Deliver any backlogged spend notifications.
	if spendChans, ok := s.spendMap[*outpoint]; ok {
		delete(s.spendMap, *outpoint)
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
