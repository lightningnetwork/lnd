package wallet

import (
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

	TheirInputs []*wire.TxIn
	OurInputs   []*wire.TxIn

	TheirChange []*wire.TxOut
	OurChange   []*wire.TxOut

	OurKey   *btcec.PrivateKey
	TheirKey *btcec.PublicKey

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	TheirSigs [][]byte
	OurSigs   [][]byte

	NormalizedTxID wire.ShaHash

	FundingTx *wire.MsgTx
	// TODO(roasbef): time locks, who pays fee etc.
	// TODO(roasbeef): record Bob's ln-ID?

	CompletedFundingTx *btcutil.Tx

	reservationID uint64
	wallet        *LightningWallet
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

// AddCounterPartyFunds...
func (r *ChannelReservation) AddFunds(theirInputs []*wire.TxIn, theirChangeOutputs []*wire.TxOut, multiSigKey *btcec.PublicKey) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartyFundsMsg{
		pendingFundingID:   r.reservationID,
		theirInputs:        theirInputs,
		theirChangeOutputs: theirChangeOutputs,
		theirKey:           multiSigKey,
	}

	return <-errChan
}

// CompleteFundingReservation...
func (r *ChannelReservation) CompleteReservation(reservationID uint64, theirSigs [][]byte) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID: reservationID,
		theirSigs:        theirSigs,
		err:              errChan,
	}

	return <-errChan
}

// RequestFundingReserveCancellation...
func (r *ChannelReservation) Cancel(reservationID uint64) {
	doneChan := make(chan struct{}, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: reservationID,
		done:             doneChan,
	}

	<-doneChan
}
