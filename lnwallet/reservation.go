package lnwallet

import (
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// ChannelContribution is the primary constituent of the funding workflow within
// lnwallet. Each side first exchanges their respective contributions along with
// channel specific paramters like the min fee/KB. Once contributions have been
// exchanged, each side will then produce signatures for all their inputs to the
// funding transactions, and finally a signature for the other party's version
// of the commitment transaction.
type ChannelContribution struct {
	// FundingOutpoint is the amount of funds contributed to the funding
	// transaction.
	FundingAmount btcutil.Amount

	// Inputs to the funding transaction.
	Inputs []*wire.TxIn

	// ChangeOutputs are the Outputs to be used in the case that the total
	// value of the fund ing inputs is greater than the total potential
	// channel capacity.
	ChangeOutputs []*wire.TxOut

	// MultiSigKey is the the key to be used for the funding transaction's
	// P2SH multi-sig 2-of-2 output.
	// TODO(roasbeef): replace with CDP
	MultiSigKey *btcec.PublicKey

	// CommitKey is the key to be used for this party's version of the
	// commitment transaction.
	CommitKey *btcec.PublicKey

	// DeliveryAddress is the address to be used for delivery of cleared
	// channel funds in the scenario of a cooperative channel closure.
	DeliveryAddress btcutil.Address

	// RevocationKey is the key to be used in the revocation clause for the
	// initial version of this party's commitment transaction.
	RevocationKey *btcec.PublicKey

	// CsvDelay The delay (in blocks) to be used for the pay-to-self output
	// in this party's version of the commitment transaction.
	CsvDelay uint32
}

// InputScripts represents any script inputs required to redeem a previous
// output. This struct is used rather than just a witness, or scripSig in
// order to accomdate nested p2sh which utilizes both types of input scripts.
type InputScript struct {
	Witness   [][]byte
	ScriptSig []byte
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
//  2. ChannelReservation.ProcessContribution/ChannelReservation.ProcessSingleContribution
//     * The counterparty presents their contribution to the payment channel.
//       This allows us to build the funding, and commitment transactions
//       ourselves.
//     * We're now able to sign our inputs to the funding transactions, and
//       the counterparty's version of the commitment transaction.
//     * All signatures crafted by us, are now available via .OurSignatures().
//  3. ChannelReservation.CompleteReservation/ChannelReservation.CompleteReservationSingle
//     * The final step in the workflow. The counterparty presents the
//       signatures for all their inputs to the funding transation, as well
//       as a signature to our version of the commitment transaction.
//     * We then verify the validity of all signatures before considering the
//       channel "open".
// TODO(roasbeef): update with single funder description
type ChannelReservation struct {
	// This mutex MUST be held when either reading or modifying any of the
	// fields below.
	sync.RWMutex

	// fundingTx is the funding transaction for this pending channel.
	fundingTx *wire.MsgTx

	// For CLTV it is nLockTime, for CSV it's nSequence, for segwit it's
	// not needed
	fundingLockTime uint32

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	ourFundingInputScripts   []*InputScript
	theirFundingInputScripts []*InputScript

	// Our signature for their version of the commitment transaction.
	ourCommitmentSig   []byte
	theirCommitmentSig []byte

	ourContribution   *ChannelContribution
	theirContribution *ChannelContribution

	partialState *channeldb.OpenChannel

	// The ID of this reservation, used to uniquely track the reservation
	// throughout its lifetime.
	reservationID uint64

	// numConfsToOpen is the number of confirmations required before the
	// channel should be considered open.
	numConfsToOpen uint16

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
func newChannelReservation(capacity, fundingAmt btcutil.Amount, minFeeRate btcutil.Amount,
	wallet *LightningWallet, id uint64, numConfs uint16) *ChannelReservation {
	var ourBalance btcutil.Amount
	var theirBalance btcutil.Amount

	if fundingAmt == 0 {
		ourBalance = 0
		theirBalance = capacity
	} else {
		ourBalance = fundingAmt
		theirBalance = capacity - fundingAmt
	}

	return &ChannelReservation{
		ourContribution: &ChannelContribution{
			FundingAmount: ourBalance,
		},
		theirContribution: &ChannelContribution{
			FundingAmount: theirBalance,
		},
		partialState: &channeldb.OpenChannel{
			Capacity:     capacity,
			OurBalance:   ourBalance,
			TheirBalance: theirBalance,
			MinFeePerKb:  minFeeRate,
			Db:           wallet.channelDB,
		},
		numConfsToOpen: numConfs,
		reservationID:  id,
		chanOpen:       make(chan *LightningChannel, 1),
		wallet:         wallet,
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

// ProcessSingleContribution verifies, and records the initiator's contribution
// to this pending single funder channel. Internally, no further action is
// taken other than recording the initiator's contribution to the single funder
// channel.
func (r *ChannelReservation) ProcessSingleContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addSingleContributionMsg{
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
func (r *ChannelReservation) OurSignatures() ([]*InputScript, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingInputScripts, r.ourCommitmentSig
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
func (r *ChannelReservation) CompleteReservation(fundingInputScripts []*InputScript,
	commitmentSig []byte) error {

	// TODO(roasbeef): add flag for watch or not?
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID:         r.reservationID,
		theirFundingInputScripts: fundingInputScripts,
		theirCommitmentSig:       commitmentSig,
		err:                      errChan,
	}

	return <-errChan
}

// CompleteReservationSingle finalizes the pending single funder channel
// reservation. Using the funding outpoint of the constructed funding transaction,
// and the initiator's signature for our version of the commitment transaction,
// we are able to verify the correctness of our committment transaction as
// crafted by the initiator. Once this method returns, our signature for the
// initiator's version of the commitment transaction is available via
// the .OurSignatures() method. As this method should only be called as a
// response to a single funder channel, only a commitment signature will be
// populated.
func (r *ChannelReservation) CompleteReservationSingle(revocationKey *btcec.PublicKey,
	fundingPoint *wire.OutPoint, commitSig []byte) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addSingleFunderSigsMsg{
		pendingFundingID:   r.reservationID,
		revokeKey:          revocationKey,
		fundingOutpoint:    fundingPoint,
		theirCommitmentSig: commitSig,
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
func (r *ChannelReservation) TheirSignatures() ([]*InputScript, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.theirFundingInputScripts, r.theirCommitmentSig
}

// FinalFundingTx returns the finalized, fully signed funding transaction for
// this reservation.
//
// NOTE: If this reservation was created as the non-initiator to a single
// funding workflow, then the full funding transaction will not be available.
// Instead we will only have the final outpoint of the funding transaction.
func (r *ChannelReservation) FinalFundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.fundingTx
}

// FundingOutpoint returns the outpoint of the funding transaction.
//
// NOTE: The pointer returned will only be set once the .ProcesContribution()
// method is called in the case of the initiator of a single funder workflow,
// and after the .CompleteReservationSingle() method is called in the case of
// a responder to a single funder workflow.
func (r *ChannelReservation) FundingOutpoint() *wire.OutPoint {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingOutpoint
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

// DispatchChan returns a channel which will be sent on once the funding
// transaction for this pending payment channel obtains the configured number
// of confirmations. Once confirmations have been obtained, a fully initialized
// LightningChannel instance is returned, allowing for channel updates.
// NOTE: If this method is called before .CompleteReservation(), it will block
// indefinitely.
func (r *ChannelReservation) DispatchChan() <-chan *LightningChannel {
	return r.chanOpen
}

// FinalizeReservation completes the pending reservation, returning an active
// open LightningChannel. This method should be called after the responder to
// the single funder workflow receives and verifies a proof from the initiator
// of an open channel.
//
// NOTE: This method should *only* be called as the last step when one is the
// responder to an initiated single funder workflow.
func (r *ChannelReservation) FinalizeReservation() (*LightningChannel, error) {
	errChan := make(chan error, 1)
	r.wallet.msgChan <- &channelOpenMsg{
		pendingFundingID: r.reservationID,
		err:              errChan,
	}

	return <-r.chanOpen, <-errChan
}
