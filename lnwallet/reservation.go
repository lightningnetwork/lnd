package lnwallet

import (
	"sync"

	"li.lan/labs/plasma/channeldb"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// ChannelContribution...
type ChannelContribution struct {
	// Amount of funds contributed to the funding transaction.
	FundingAmount btcutil.Amount

	// Inputs to the funding transaction.
	Inputs []*wire.TxIn

	// Outputs to be used in the case that the total value of the fund
	// ing inputs is greather than the total potential channel capacity.
	ChangeOutputs []*wire.TxOut

	// The key to be used for the funding transaction's P2SH multi-sig
	// 2-of-2 output.
	MultiSigKey *btcec.PublicKey

	// The key to be used for this party's version of the commitment
	// transaction.
	CommitKey *btcec.PublicKey

	// Address to be used for delivery of cleared channel funds in the scenario
	// of a cooperative channel closure.
	DeliveryAddress btcutil.Address

	// Hash to be used as the revocation for the initial version of this
	// party's commitment transaction.
	RevocationHash [20]byte

	// The delay (in blocks) to be used for the pay-to-self output in this
	// party's version of the commitment transaction.
	CsvDelay uint32
}

// ChannelReservation...
type ChannelReservation struct {
	fundingType FundingType

	// This mutex MUST be held when either reading or modifying any of the
	// fields below.
	sync.RWMutex

	// For CLTV it is nLockTime, for CSV it's nSequence, for segwit it's
	// not needed
	fundingLockTime uint32

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	ourFundingSigs   [][]byte
	theirFundingSigs [][]byte

	// Our signature for their version of the commitment transaction.
	ourCommitmentSig   []byte
	theirCommitmentSig []byte

	ourContribution   *ChannelContribution
	theirContribution *ChannelContribution

	partialState *channeldb.OpenChannel

	// The ID of this reservation, used to uniquely track the reservation
	// throughout its lifetime.
	reservationID uint64

	// A channel which will be sent on once the channel is considered
	// 'open'. A channel is open once the funding transaction has reached
	// a sufficient number of confirmations.
	chanOpen chan *LightningChannel

	wallet *LightningWallet
}

// newChannelReservation...
func newChannelReservation(t FundingType, fundingAmt btcutil.Amount,
	minFeeRate btcutil.Amount, wallet *LightningWallet, id uint64) *ChannelReservation {
	// TODO(roasbeef): CSV here, or on delay?
	return &ChannelReservation{
		fundingType: t,
		ourContribution: &ChannelContribution{
			FundingAmount: fundingAmt,
		},
		theirContribution: &ChannelContribution{
			FundingAmount: fundingAmt,
		},
		partialState: &channeldb.OpenChannel{
			// TODO(roasbeef): assumes balanced symmetric channels.
			Capacity:     fundingAmt * 2,
			OurBalance:   fundingAmt,
			TheirBalance: fundingAmt,
			MinFeePerKb:  minFeeRate,
		},
		reservationID: id,
		wallet:        wallet,
	}
}

// OurContribution...
// NOTE: This SHOULD NOT be modified.
func (r *ChannelReservation) OurContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.ourContribution
}

// ProcessContribution...
func (r *ChannelReservation) ProcessContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addContributionMsg{
		pendingFundingID: r.reservationID,
		contribution:     theirContribution,
		err:              errChan,
	}

	return <-errChan
}

func (r *ChannelReservation) TheirContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.theirContribution
}

// OurSignatures...
func (r *ChannelReservation) OurSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingSigs, r.ourCommitmentSig
}

// CompleteFundingReservation...
func (r *ChannelReservation) CompleteReservation(fundingSigs [][]byte,
	commitmentSig []byte) error {

	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID:   r.reservationID,
		theirFundingSigs:   fundingSigs,
		theirCommitmentSig: commitmentSig,
		err:                errChan,
	}

	return <-errChan
}

// OurSignatures...
func (r *ChannelReservation) TheirSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.theirFundingSigs, r.theirCommitmentSig
}

// FinalFundingTransaction...
func (r *ChannelReservation) FinalFundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingTx
}

// RequestFundingReserveCancellation...
func (r *ChannelReservation) Cancel() error {
	errChan := make(chan error, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: r.reservationID,
		err:              errChan,
	}

	return <-errChan
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
