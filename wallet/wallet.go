package wallet

import (
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

var (
	// Namespace bucket keys.
	lightningNamespaceKey = []byte("lightning")

	// Error types
	ErrInsufficientFunds = errors.New("not enough available outputs to create funding transaction")
)

// FundingRequest...
type FundingReserveRequest struct {
	fundingAmount btcutil.Amount

	// Insuffcient funds etc..
	err chan error // Buffered

	resp chan *FundingReserveResponse // Buffered
}

// FundingResponse...
type FundingReserveResponse struct {
	fundingInputs []*wire.TxIn
	changeOutputs []*wire.TxOut
}

// FundingReserveCancelMsg...
type FundingReserveCancelMsg struct {
	reserveToCancel *FundingReserveResponse
}

// FundingReserveRequest...
type FundingCompleteRequest struct {
	fundingReservation *FundingReserveResponse

	theirInputs        []*wire.TxIn // Includes sig-script to redeem.
	theirChangeOutputs []*wire.TxOut
	theirKey           *btcec.PublicKey

	err chan error // Buffered

	resp chan FundingCompleteResponse // Buffered
}

// FundingCompleteResponse....
type FundingCompleteResponse struct {
	fundingTx *btcutil.Tx
}

// LightningWallet....
// responsible for internal global (from the point of view of a user/node)
// channel state. Requests to modify this state come in via messages over
// channels, same with replies.
// Embedded wallet backed by boltdb...
type LightningWallet struct {
	sync.RWMutex

	db *walletdb.DB

	// A namespace within boltdb reserved for ln-based wallet meta-data.
	// TODO(roasbeef): which possible other namespaces are relevant?
	lnNamespace *walletdb.Namespace

	btcwallet.Wallet

	msgChan chan interface{}

	//lockedInputs  []*LockedPrevOut
	//lockedOutputs []*LockedOutPoint
	keyPool *multiSigKeyPool

	started  int32
	shutdown int32

	quit chan struct{}

	wg sync.WaitGroup
}

// Start...
func (l *LightningWallet) Start() error {
	// Already started?
	if atomic.AddInt32(&l.started, 1) != 1 {
		return nil
	}

	l.wg.Add(1)
	go l.requestHandler()

	return nil
}

// Stop...
func (l *LightningWallet) Stop() error {
	if atomic.AddInt32(&l.shutdown, 1) != 1 {
		return nil
	}

	close(l.quit)
	l.wg.Wait()
	return nil
}

// requestHandler....
func (l *LightningWallet) requestHandler() {
out:
	for {
		select {
		case m := <-l.msgChan:
			switch msg := m.(type) {
			case *FundingReserveRequest:
				l.handleFundingReserveRequest(msg)
			case *FundingReserveCancelMsg:
				l.handleFundingCancelRequest(msg)
			}
		case <-l.quit:
			// TODO: do some clean up
			break out
		}
	}

	l.wg.Done()
}

// handleFundingReserveRequest...
func (l *LightningWallet) handleFundingReserveRequest(req *FundingReserveRequest) {
	fundingAmt := req.fundingAmount

	// Find all unlocked unspent outputs with greater than 6 confirmations.
	maxConfs := ^int32(0)
	unspentOutputs, err := l.ListUnspent(6, maxConfs, nil)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// Convert the outputs to coins for coin selection below.
	coins, err := outputsToCoins(unspentOutputs)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// Peform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount requirements.
	//
	// TODO(roasbeef): Should extend coinset with optimal coin selection
	// heuristics for our use case.
	// TODO(roasbeef): factor in fees..
	// NOTE: this current selection assumes "priority" is still a thing.
	selector := &coinset.MaxValueAgeCoinSelector{
		MaxInputs:       10,
		MinChangeAmount: 10000,
	}
	selectedCoins, err := selector.CoinSelect(fundingAmt, coins)

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	fundingInputs := make([]*wire.TxIn, len(selectedCoins.Coins()))
	for i, coin := range selectedCoins.Coins() {
		txout := wire.NewOutPoint(coin.Hash(), coin.Index())
		l.LockOutpoint(*txout)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		outPoint := wire.NewOutPoint(coin.Hash(), coin.Index())
		fundingInputs[i] = wire.NewTxIn(outPoint, nil)
	}

	// Create some possibly neccessary change outputs.
	selectedTotalValue := coinset.NewCoinSet(coins).TotalValue()
	changeOutputs := make([]*wire.TxOut, 0, len(selectedCoins.Coins()))
	if selectedTotalValue > fundingAmt {
		// Change is necessary. Query for an available change address to
		// send the remainder to.
		changeAmount := selectedTotalValue - fundingAmt
		changeAddr, err := l.NewChangeAddress(waddrmgr.DefaultAccountNum)
		if err != nil {
			req.err <- err
			req.resp <- nil
		}

		changeOutputs = append(changeOutputs,
			wire.NewTxOut(int64(changeAmount), changeAddr.ScriptAddress()))
	}

	// Funding reservation request succesfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or cancecled.
	req.err <- nil
	req.resp <- &FundingReserveResponse{
		fundingInputs: fundingInputs,
		changeOutputs: changeOutputs,
	}
}

// lnCoin...
// to adhere to the coinset.Coin interface
type lnCoin struct {
	hash     *wire.ShaHash
	index    uint32
	value    btcutil.Amount
	pkScript []byte
	numConfs int64
	valueAge int64
}



	}

	if err != nil {
	}


		if err != nil {
		}

	}

}

// handleFundingReserveCancel...
func (l *LightningWallet) handleFundingCancelRequest(req *FundingReserveCancelMsg) {
	prevReservation := req.reserveToCancel

	for _, unusedInput := range prevReservation.fundingInputs {
		l.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): Is it possible to mark the unused change also as
	// available?
}
