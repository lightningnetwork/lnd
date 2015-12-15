package wallet

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// ChannelReservation...
type ChannelReservation struct {
	FundingType FundingType

	FundingAmount btcutil.Amount
	ReserveAmount btcutil.Amount
	MinFeePerKb   btcutil.Amount

	sync.RWMutex // All fields below owned by the lnwallet.

	theirInputs []*wire.TxIn
	ourInputs   []*wire.TxIn

	theirChange []*wire.TxOut
	ourChange   []*wire.TxOut

	ourKey   *btcec.PrivateKey
	theirKey *btcec.PublicKey

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	theirSigs [][]byte
	ourSigs   [][]byte

	normalizedTxID wire.ShaHash

	fundingTx *wire.MsgTx
	// TODO(roasbef): time locks, who pays fee etc.
	// TODO(roasbeef): record Bob's ln-ID?

	completedFundingTx *btcutil.Tx

	reservationID uint64
	wallet        *LightningWallet

	chanOpen chan *LightningChannel
}

// newChannelReservation...
func newChannelReservation(t FundingType, fundingAmt btcutil.Amount,
	minFeeRate btcutil.Amount, wallet *LightningWallet, id uint64) *ChannelReservation {
	return &ChannelReservation{
		FundingType:   t,
		FundingAmount: fundingAmt,
		MinFeePerKb:   minFeeRate,
		wallet:        wallet,
		reservationID: id,
	}
}

// OurFunds...
func (r *ChannelReservation) OurFunds() ([]*wire.TxIn, []*wire.TxOut, *btcec.PublicKey) {
	r.RLock()
	defer r.RUnlock()
	return r.ourInputs, r.ourChange, r.ourKey.PubKey()
}

// AddCounterPartyFunds...
func (r *ChannelReservation) AddFunds(theirInputs []*wire.TxIn, theirChangeOutputs []*wire.TxOut, multiSigKey *btcec.PublicKey) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartyFundsMsg{
		pendingFundingID:   r.reservationID,
		theirInputs:        theirInputs,
		theirChangeOutputs: theirChangeOutputs,
		theirKey:           multiSigKey,
		err:                errChan,
	}

	return <-errChan
}

// OurSigs...
func (r *ChannelReservation) OurSigs() [][]byte {
	r.RLock()
	defer r.RUnlock()
	return r.ourSigs
}

// TheirFunds...
// TODO(roasbeef): return error if accessors not yet populated?
func (r *ChannelReservation) TheirFunds() ([]*wire.TxIn, []*wire.TxOut, *btcec.PublicKey) {
	r.RLock()
	defer r.RUnlock()
	return r.theirInputs, r.theirChange, r.theirKey
}

// CompleteFundingReservation...
func (r *ChannelReservation) CompleteReservation(theirSigs [][]byte) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID: r.reservationID,
		theirSigs:        theirSigs,
		err:              errChan,
	}

	return <-errChan
}

// FinalFundingTransaction...
func (r *ChannelReservation) FinalFundingTx() *btcutil.Tx {
	r.RLock()
	defer r.RUnlock()
	return r.completedFundingTx
}

// RequestFundingReserveCancellation...
// TODO(roasbeef): also return mutated state?
func (r *ChannelReservation) Cancel() {
	doneChan := make(chan struct{}, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: r.reservationID,
		done:             doneChan,
	}

	<-doneChan
}

// WaitForChannelOpen...
func (r *ChannelReservation) WaitForChannelOpen() *LightningChannel {
	return nil
}

// * finish reset of tests
// * comment out stuff that'll need a node.
// * start on commitment side
//   * implement rusty's shachain
//   * set up logic to get notification from node when funding tx gets 6 deep.
//     * prob spawn into ChainNotifier struct
//   * create builder for initial funding transaction
//     * fascade through the wallet, for signing and such.
//   * channel should have active namespace to it's bucket, query at that point fo past commits etc
