package lnwallet

import (
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// ChannelContribution is the primary constituent of the funding workflow within
// lnwallet. Each side first exchanges their respective contributions along with
// channel specific paramters like the min fee/KB. Once contributions have been
// exchanged, each side will then produce signatures for all their inputs to the
// funding transactions, and finally a signature for the other party's version
// of the commitment transaction.
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

// ChannelReservation represents an intent to open a lightning payment channel
// a counterpaty. The funding proceses from reservation to channel opening is a
// 3-step process. In order to allow for full concurrency during the reservation
// workflow, resources consumed by a contribution are "locked" themselves. This
// prevents a number of race conditions such as two funding transactions
// double-spending the same input. A reservation can also be cancelled, which
// removes the resources from limbo, allowing another reservation to claim them.
//
// The reservation workflow consists of the following three steps:
//  1. lnwallet.InitChannelReservation
//     * One requests the wallet to allocate the neccessary resources for a
//      channel reservation. These resources a put in limbo for the lifetime
//      of a reservation.
//    * Once completed the reservation will have the wallet's contribution
//      accessible via the .OurContribution() method. This contribution
//      contains the neccessary items to allow the remote party to build both
//      the funding, and commitment transactions.
//  2. ChannelReservation.ProcessContribution
//     * The counterparty presents their contribution to the payment channel.
//       This allows us to build the funding, and commitment transactions
//       ourselves.
//     * We're now able to sign our inputs to the funding transactions, and
//       the counterparty's version of the commitment transaction.
//     * All signatures crafted by us, are now available via .OurSignatures().
//  3. ChannelReservation.CompleteReservation
//     * The final step in the workflow. The counterparty presents the
//       signatures for all their inputs to the funding transation, as well
//       as a signature to our version of the commitment transaction.
//     * We then verify the validity of all signatures before considering the
//       channel "open".
type ChannelReservation struct {
	// TODO(roasbeef): remove this? we're only implementing the golden...
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

// newChannelReservation creates a new channel reservation. This function is
// used only internally by lnwallet. In order to concurrent safety, the creation
// of all channel reservations should be carried out via the
// lnwallet.InitChannelReservation interface.
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
			Db:           wallet.channelDB,
		},
		reservationID: id,
		wallet:        wallet,
	}
}

// OurContribution returns the wallet's fully populated contribution to the
// pending payment channel. See 'ChannelContribution' for further details
// regarding the contents of a contribution.
// NOTE: This SHOULD NOT be modified.
// TODO(roasbeef): make copy?
func (r *ChannelReservation) OurContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.ourContribution
}

// ProcesContribution verifies the counterparty's contribution to the pending
// payment channel. As a result of this incoming message, lnwallet is able to
// build the funding transaction, and both commitment transactions. Once this
// message has been processed, all signatures to inputs to the funding
// transaction belonging to the wallet are available. Additionally, the wallet
// will generate a signature to the counterparty's version of the commitment
// transaction.
func (r *ChannelReservation) ProcessContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addContributionMsg{
		pendingFundingID: r.reservationID,
		contribution:     theirContribution,
		err:              errChan,
	}

	return <-errChan
}

// TheirContribution returns the counterparty's pending contribution to the
// payment channel. See 'ChannelContribution' for further details regarding
// the contents of a contribution. This attribute will ONLY be available
// after a call to .ProcesContribution().
// NOTE: This SHOULD NOT be modified.
func (r *ChannelReservation) TheirContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.theirContribution
}

// OurSignatures retrieves the wallet's signatures to all inputs to the funding
// transaction belonging to itself, and also a signature for the counterparty's
// version of the commitment transaction. The signatures for the wallet's
// inputs to the funding transaction are returned in sorted order according to
// BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
// NOTE: These signatures will only be populated after a call to
// .ProcesContribution()
func (r *ChannelReservation) OurSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingSigs, r.ourCommitmentSig
}

// CompleteFundingReservation finalizes the pending channel reservation,
// transitioning from a pending payment channel, to an open payment
// channel. All passed signatures to the counterparty's inputs to the funding
// transaction will be fully verified. Signatures are expected to be passed in
// sorted order according to BIP-69:
// https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki. Additionally,
// verification is performed in order to ensure that the counterparty supplied
// a valid signature to our version of the commitment transaction.
// Once this method returns, caller's should then call .WaitForChannelOpen()
// which will block until the funding transaction obtains the configured number
// of confirmations. Once the method unblocks, a LightningChannel instance is
// returned, marking the channel available for updates.
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

// OurSignatures returns the counterparty's signatures to all inputs to the
// funding transaction belonging to them, as well as their signature for the
// wallet's version of the commitment transaction. This methods is provided for
// additional verification, such as needed by tests.
// NOTE: These attributes will be unpopulated before a call to
// .CompleteReservation().
func (r *ChannelReservation) TheirSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.theirFundingSigs, r.theirCommitmentSig
}

// FinalFundingTx returns the finalized, fully signed funding transaction for
// this reservation.
func (r *ChannelReservation) FinalFundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingTx
}

// Cancel abandons this channel reservation. This method should be called in
// the scenario that communications with the counterparty break down. Upon
// cancellation, all resources previously reserved for this pending payment
// channel are returned to the free pool, allowing subsequent reservations to
// utilize the now freed resources.
func (r *ChannelReservation) Cancel() error {
	errChan := make(chan error, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: r.reservationID,
		err:              errChan,
	}

	return <-errChan
}

// WaitForChannelOpen blocks until the funding transaction for this pending
// payment channel obtains the configured number of confirmations. Once
// confirmations have been obtained, a fully initialized LightningChannel
// instance is returned, allowing for channel updates.
// NOTE: If this method is called before .CompleteReservation(), it will block
// indefinitely.
func (r *ChannelReservation) WaitForChannelOpen() *LightningChannel {
	return <-r.chanOpen
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
