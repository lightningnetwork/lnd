package lnwallet

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/chainntfs"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

var (
	ErrChanClosing = fmt.Errorf("channel is being closed, operation disallowed")
)

const (
	// TODO(roasbeef): make not random value
	MaxPendingPayments = 10
)

// channelState is an enum like type which represents the current state of a
// particular channel.
type channelState uint8

const (
	// channelPending indicates this channel is still going through the
	// funding workflow, and isn't yet open.
	channelPending channelState = iota

	// channelOpen represents an open, active channel capable of
	// sending/receiving HTLCs.
	channelOpen

	// channelClosing represents a channel which is in the process of being
	// closed.
	channelClosing

	// channelClosed represents a channel which has been fully closed. Note
	// that before a channel can be closed, ALL pending HTLC's must be
	// settled/removed.
	channelClosed

	// channelDispute indicates that an un-cooperative closure has been
	// detected within the channel.
	channelDispute

	// channelPendingPayment indicates that there a currently outstanding
	// HTLC's within the channel.
	channelPendingPayment
)

// PaymentHash presents the hash160 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [32]byte

// LightningChannel...
// TODO(roasbeef): future peer struct should embed this struct
type LightningChannel struct {
	lnwallet      *LightningWallet
	channelEvents chainntnfs.ChainNotifier

	sync.RWMutex
	status channelState

	// TODO(roasbeef): Stores all previous R values + timeouts for each
	// commitment update, plus some other meta-data...Or just use OP_RETURN
	// to help out?
	// currently going for: nSequence/nLockTime overloading
	channelDB *channeldb.DB

	// stateMtx protects concurrent access to the state struct.
	stateMtx     sync.RWMutex
	channelState *channeldb.OpenChannel

	updateTotem chan struct{}

	// Uncleared HTLC's.
	pendingPayments map[PaymentHash]*PaymentDescriptor

	// Payment's which we've requested.
	unfufilledPayments map[PaymentHash]*PaymentRequest

	fundingTxIn *wire.TxIn
	fundingP2SH []byte

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewLightningChannel...
func NewLightningChannel(wallet *LightningWallet, events chainntnfs.ChainNotifier,
	chanDB *channeldb.DB, state *channeldb.OpenChannel) (*LightningChannel, error) {

	lc := &LightningChannel{
		lnwallet:           wallet,
		channelEvents:      events,
		channelState:       state,
		channelDB:          chanDB,
		updateTotem:        make(chan struct{}, 1),
		pendingPayments:    make(map[PaymentHash]*PaymentDescriptor),
		unfufilledPayments: make(map[PaymentHash]*PaymentRequest),
	}

	// TODO(roasbeef): do a NotifySpent for the funding input, and
	// NotifyReceived for all commitment outputs.

	// Populate the totem.
	lc.updateTotem <- struct{}{}

	fundingPkScript, err := witnessScriptHash(state.FundingRedeemScript)
	if err != nil {
		return nil, err
	}
	lc.fundingTxIn = wire.NewTxIn(state.FundingOutpoint, nil, nil)
	lc.fundingP2SH = fundingPkScript

	return lc, nil
}

// PaymentDescriptor...
type PaymentDescriptor struct {
	RHash   [32]byte
	Timeout uint32
	Value   btcutil.Amount

	OurRevocation   [32]byte // TODO(roasbeef): don't need these?
	TheirRevocation [32]byte

	PayToUs bool
}

// ChannelUpdate...
type ChannelUpdate struct {
	pendingDesc *PaymentDescriptor
	deletion    bool

	currentUpdateNum uint64
	pendingUpdateNum uint64

	ourPendingCommitTx   *wire.MsgTx
	theirPendingCommitTx *wire.MsgTx

	pendingRevocation [32]byte
	sigTheirNewCommit []byte

	// TODO(roasbeef): some enum to track current state in lifetime?
	// state UpdateStag

	lnChannel *LightningChannel
}

// RevocationHash...
func (c *ChannelUpdate) RevocationHash() ([]byte, error) {
	c.lnChannel.stateMtx.RLock()
	defer c.lnChannel.stateMtx.RUnlock()

	e := c.lnChannel.channelState.LocalElkrem
	nextPreimage, err := e.AtIndex(c.pendingUpdateNum)
	if err != nil {
		return nil, err
	}

	return nextPreimage[:], nil
}

// SignCounterPartyCommitment...
func (c *ChannelUpdate) SignCounterPartyCommitment() ([]byte, error) {
	c.lnChannel.stateMtx.RLock()
	defer c.lnChannel.stateMtx.RUnlock()

	if c.sigTheirNewCommit != nil {
		return c.sigTheirNewCommit, nil
	}

	// Sign their version of the commitment transaction.
	hashCache := txscript.NewTxSigHashes(c.theirPendingCommitTx)
	sig, err := txscript.RawTxInWitnessSignature(c.theirPendingCommitTx,
		hashCache, 0, int64(c.lnChannel.channelState.Capacity),
		c.lnChannel.channelState.FundingRedeemScript, txscript.SigHashAll,
		c.lnChannel.channelState.OurMultiSigKey)
	if err != nil {
		return nil, err
	}

	c.sigTheirNewCommit = sig

	return sig, nil
}

// PreviousRevocationPreImage...
func (c *ChannelUpdate) PreviousRevocationPreImage() ([]byte, error) {
	c.lnChannel.stateMtx.RLock()
	defer c.lnChannel.stateMtx.RUnlock()

	// Retrieve the pre-image to the revocation hash our current commitment
	// transaction.
	e := c.lnChannel.channelState.LocalElkrem
	revokePreImage, err := e.AtIndex(c.currentUpdateNum)
	if err != nil {
		return nil, err
	}

	return revokePreImage[:], nil
}

// VerifyNewCommitmentSigs...
func (c *ChannelUpdate) VerifyNewCommitmentSigs(ourSig, theirSig []byte) error {
	c.lnChannel.stateMtx.RLock()
	defer c.lnChannel.stateMtx.RUnlock()

	channelState := c.lnChannel.channelState

	// TODO(roasbeef): replace with sighash calc and regular sig check
	// after merge

	// When initially generating the redeemScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virual Machine stack in
	// the correct order.
	redeemScript := channelState.FundingRedeemScript
	ourKey := channelState.OurCommitKey.PubKey().SerializeCompressed()
	theirKey := channelState.TheirCommitKey.SerializeCompressed()
	witness := spendMultiSig(redeemScript, ourKey, ourSig, theirKey, theirSig)

	// Attach the scriptSig to our commitment transaction's only input,
	// then validate that the scriptSig executes correctly.
	commitTx := c.ourPendingCommitTx
	commitTx.TxIn[0].Witness = witness
	// TODO(roasbeef): need hashcache and value here
	vm, err := txscript.NewEngine(c.lnChannel.fundingP2SH, commitTx, 0,
		txscript.StandardVerifyFlags, nil, nil, 0)
	if err != nil {
		return err
	}

	return vm.Execute()
}

// Commit...
func (c *ChannelUpdate) Commit(pastRevokePreimage []byte) error {
	c.lnChannel.stateMtx.Lock()
	defer c.lnChannel.stateMtx.Unlock()

	// First, ensure that the pre-image properly links into the shachain.
	//theirShaChain := c.lnChannel.channelState.TheirShaChain
	//var preImage [32]byte
	//copy(preImage[:], pastRevokePreimage)
	//if err := theirShaChain.AddNextHash(preImage); err != nil {
	//	return err
	//}

	channelState := c.lnChannel.channelState

	// Finally, verify that that this is indeed the pre-image to the
	// revocation hash we were given earlier.
	if !bytes.Equal(btcutil.Hash160(pastRevokePreimage),
		channelState.TheirCurrentRevocation[:]) {
		return fmt.Errorf("pre-image hash does not match revocation")
	}

	// Store this current revocation in the channel state so we can
	// verify future channel updates.
	channelState.TheirCurrentRevocation = c.pendingRevocation

	// The channel update is now complete, roll over to the newest commitment
	// transaction.
	channelState.OurCommitTx = c.ourPendingCommitTx
	channelState.TheirCommitTx = c.theirPendingCommitTx
	channelState.NumUpdates = c.pendingUpdateNum

	// If this channel update involved deleting an HTLC, remove it from the
	// set of pending payments.
	if c.deletion {
		delete(c.lnChannel.pendingPayments, c.pendingDesc.RHash)
	}

	// TODO(roasbeef): db writes, checkpoints, and such

	// Return the updateTotem, allowing another update to be created now
	// that this pending update has been commited, and finalized.
	c.lnChannel.updateTotem <- struct{}{}

	return nil
}

// ChannelPoint returns the outpoint of the original funding transaction which
// created this active channel. This outpoint is used throughout various
// sub-systems to uniquely identify an open channel.
func (lc *LightningChannel) ChannelPoint() *wire.OutPoint {
	return lc.channelState.ChanID
}

// AddHTLC...
// 1. request R_Hash from receiver (only if single hop, would be out of band)
// 2. propose HTLC
//    * timeout
//    * value
//    * r_hash
//    * next revocation hash
// 3. they accept
//    * their next revocation hash
//    * their sig for our new commitment tx (verify correctness)
// Can buld both new commitment txns at this point
// 4. we give sigs
//    * our sigs for their new commitment tx
//    * the pre-image to our old commitment tx
// 5. they complete
//    * the pre-image to their old commitment tx (verify is part of their chain, is pre-image)
func (lc *LightningChannel) AddHTLC(timeout uint32, value btcutil.Amount,
	rHash, revocation PaymentHash, payToUs bool) (*ChannelUpdate, error) {

	// Grab the updateTotem, this acts as a barrier upholding the invariant
	// that only one channel update transaction should exist at any moment.
	// This aides in ensuring the channel updates are atomic, and consistent.
	<-lc.updateTotem

	chanUpdate := &ChannelUpdate{
		pendingDesc: &PaymentDescriptor{
			RHash:           rHash,
			TheirRevocation: revocation,
			Timeout:         timeout,
			Value:           value,
			PayToUs:         payToUs,
		},
		pendingRevocation: revocation,
		lnChannel:         lc,
	}

	// Get next revocation hash, updating the number of updates in the
	// channel as a result.
	chanUpdate.currentUpdateNum = lc.channelState.NumUpdates
	chanUpdate.pendingUpdateNum = lc.channelState.NumUpdates + 1
	nextPreimage, err := lc.channelState.LocalElkrem.AtIndex(chanUpdate.pendingUpdateNum)
	if err != nil {
		return nil, err
	}
	copy(chanUpdate.pendingDesc.OurRevocation[:], btcutil.Hash160(nextPreimage[:]))

	// Re-calculate the amount of cleared funds for each side.
	var amountToUs, amountToThem btcutil.Amount
	if payToUs {
		amountToUs = lc.channelState.OurBalance
		amountToThem = lc.channelState.TheirBalance - value
	} else {
		amountToUs = lc.channelState.OurBalance - value
		amountToThem = lc.channelState.TheirBalance
	}

	// Re-create copies of the current commitment transactions to be updated.
	ourNewCommitTx, theirNewCommitTx, err := createNewCommitmentTxns(
		lc.fundingTxIn, lc.channelState, chanUpdate, amountToUs, amountToThem,
	)
	if err != nil {
		return nil, err
	}

	// First, re-add all the old HTLCs.
	for _, paymentDesc := range lc.pendingPayments {
		if err := lc.addHTLC(ourNewCommitTx, theirNewCommitTx, paymentDesc); err != nil {
			return nil, err
		}
	}

	// Then add this new HTLC.
	if err := lc.addHTLC(ourNewCommitTx, theirNewCommitTx, chanUpdate.pendingDesc); err != nil {
		return nil, err
	}
	lc.pendingPayments[rHash] = chanUpdate.pendingDesc // TODO(roasbeef): check for dups?

	// Sort both transactions according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourNewCommitTx)
	txsort.InPlaceSort(theirNewCommitTx)

	// TODO(roasbeef): locktimes/sequence set

	// TODO(roasbeef): write checkpoint here...

	chanUpdate.ourPendingCommitTx = ourNewCommitTx
	chanUpdate.theirPendingCommitTx = theirNewCommitTx

	return chanUpdate, nil
}

// addHTLC...
// NOTE: This MUST be called with stateMtx held.
func (lc *LightningChannel) addHTLC(ourCommitTx, theirCommitTx *wire.MsgTx,
	paymentDesc *PaymentDescriptor) error {

	// If the HTLC is going to us, then we're the sender, otherwise they
	// are.
	var senderKey, receiverKey *btcec.PublicKey
	var senderRevocation, receiverRevocation []byte
	if paymentDesc.PayToUs {
		receiverKey = lc.channelState.OurCommitKey.PubKey()
		receiverRevocation = paymentDesc.OurRevocation[:]
		senderKey = lc.channelState.TheirCommitKey
		senderRevocation = paymentDesc.TheirRevocation[:]
	} else {
		senderKey = lc.channelState.OurCommitKey.PubKey()
		senderRevocation = paymentDesc.OurRevocation[:]
		receiverKey = lc.channelState.TheirCommitKey
		receiverRevocation = paymentDesc.TheirRevocation[:]
	}

	// Generate the proper redeem scripts for the HTLC output for both the
	// sender and the receiver.
	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash
	delay := lc.channelState.LocalCsvDelay
	senderPKScript, err := senderHTLCScript(timeout, delay, senderKey,
		receiverKey, senderRevocation[:], rHash[:])
	if err != nil {
		return nil
	}
	receiverPKScript, err := receiverHTLCScript(timeout, delay, senderKey,
		receiverKey, receiverRevocation[:], rHash[:])
	if err != nil {
		return nil
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for each.
	senderP2SH, err := witnessScriptHash(senderPKScript)
	if err != nil {
		return nil
	}
	receiverP2SH, err := witnessScriptHash(receiverPKScript)
	if err != nil {
		return nil
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Value)
	if paymentDesc.PayToUs {
		ourCommitTx.AddTxOut(wire.NewTxOut(amountPending, receiverP2SH))
		theirCommitTx.AddTxOut(wire.NewTxOut(amountPending, senderP2SH))
	} else {
		ourCommitTx.AddTxOut(wire.NewTxOut(amountPending, senderP2SH))
		theirCommitTx.AddTxOut(wire.NewTxOut(amountPending, receiverP2SH))
	}

	return nil
}

// SettleHTLC...
// R-VALUE, NEW REVOKE HASH
// accept, sig
func (lc *LightningChannel) SettleHTLC(rValue [32]byte, newRevocation [32]byte) (*ChannelUpdate, error) {
	// Grab the updateTotem, this acts as a barrier upholding the invariant
	// that only one channel update transaction should exist at any moment.
	// This aides in ensuring the channel updates are atomic, and consistent.
	<-lc.updateTotem

	// Find the matching payment descriptor, bailing out early if it
	// doesn't exist.
	var rHash PaymentHash
	copy(rHash[:], btcutil.Hash160(rValue[:]))
	payDesc, ok := lc.pendingPayments[rHash]
	if !ok {
		return nil, fmt.Errorf("r-hash for preimage not found")
	}

	chanUpdate := &ChannelUpdate{
		pendingDesc:       payDesc,
		deletion:          true,
		pendingRevocation: newRevocation,
		lnChannel:         lc,
	}

	// TODO(roasbeef): such copy pasta, make into func...
	// Get next revocation hash, updating the number of updates in the
	// channel as a result.
	chanUpdate.currentUpdateNum = lc.channelState.NumUpdates
	chanUpdate.pendingUpdateNum = lc.channelState.NumUpdates + 1
	nextPreimage, err := lc.channelState.LocalElkrem.AtIndex(chanUpdate.pendingUpdateNum)
	if err != nil {
		return nil, err
	}
	copy(chanUpdate.pendingDesc.OurRevocation[:], btcutil.Hash160(nextPreimage[:]))

	// Re-calculate the amount of cleared funds for each side.
	var amountToUs, amountToThem btcutil.Amount
	if payDesc.PayToUs {
		amountToUs = lc.channelState.OurBalance + payDesc.Value
		amountToThem = lc.channelState.TheirBalance
	} else {
		amountToUs = lc.channelState.OurBalance
		amountToThem = lc.channelState.TheirBalance + payDesc.Value
	}

	// Create new commitment transactions that reflect the settlement of
	// this pending HTLC.
	ourNewCommitTx, theirNewCommitTx, err := createNewCommitmentTxns(
		lc.fundingTxIn, lc.channelState, chanUpdate, amountToUs, amountToThem,
	)
	if err != nil {
		return nil, err
	}

	// Re-add all the HTLC's skipping over this newly settled payment.
	for paymentHash, paymentDesc := range lc.pendingPayments {
		if bytes.Equal(paymentHash[:], rHash[:]) {
			continue
		}
		if err := lc.addHTLC(ourNewCommitTx, theirNewCommitTx, paymentDesc); err != nil {
			return nil, err
		}
	}

	// Sort both transactions according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourNewCommitTx)
	txsort.InPlaceSort(theirNewCommitTx)

	// TODO(roasbeef): locktimes/sequence set

	// TODO(roasbeef): write checkpoint here...

	chanUpdate.ourPendingCommitTx = ourNewCommitTx
	chanUpdate.theirPendingCommitTx = theirNewCommitTx

	return chanUpdate, nil
}

// CancelHTLC...
func (lc *LightningChannel) CancelHTLC() error {
	return nil
}

// ForceClose...
func (lc *LightningChannel) ForceClose() error {
	return nil
}

// InitCooperativeClose initiates a cooperative closure of an active lightning
// channel. This method should only be executed once all pending HTLCs (if any)
// on the channel have been cleared/removed. Upon completion, the source channel
// will shift into the "closing" state, which indicates that all incoming/outgoing
// HTLC requests should be rejected. A signature for the closing transaction,
// and the txid of the closing transaction are returned. The initiator of the
// channel closure should then watch the blockchain for a confirmation of the
// closing transaction before considering the channel terminated. In the case
// of an unresponsive remote party, the initiator can either choose to execute
// a force closure, or backoff for a period of time, and retry the cooperative
// closure.
// TODO(roasbeef): caller should initiate signal to reject all incoming HTLCs,
// settle any inflight.
func (lc *LightningChannel) InitCooperativeClose() ([]byte, *wire.ShaHash, error) {
	lc.Lock()
	defer lc.Unlock() // TODO(roasbeef): coarser graiend locking

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, nil, ErrChanClosing
	}

	// Otherwise, indicate in the channel status that a channel closure has
	// been initiated.
	lc.status = channelClosing

	// TODO(roasbeef): assumes initiator pays fees
	closeTx := createCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		true)
	closeTxSha := closeTx.TxSha()

	// Finally, sign the completed cooperative closure transaction. As the
	// initiator we'll simply send our signature over the the remote party,
	// using the generated txid to be notified once the closure transaction
	// has been confirmed.
	hashCache := txscript.NewTxSigHashes(closeTx)
	closeSig, err := txscript.RawTxInWitnessSignature(closeTx,
		hashCache, 0, int64(lc.channelState.Capacity),
		lc.channelState.FundingRedeemScript, txscript.SigHashAll,
		lc.channelState.OurMultiSigKey)
	if err != nil {
		return nil, nil, err
	}

	return closeSig, &closeTxSha, nil
}

// CompleteCooperativeClose completes the cooperative closure of the target
// active lightning channel. This method should be called in response to the
// remote node initating a cooperative channel closure. A fully signed closure
// transaction is returned. It is the duty of the responding node to broadcast
// a signed+valid closure transaction to the network.
func (lc *LightningChannel) CompleteCooperativeClose(remoteSig []byte) (*wire.MsgTx, error) {
	lc.Lock()
	defer lc.Unlock() // TODO(roasbeef): coarser graiend locking

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, ErrChanClosing
	}

	lc.status = channelClosed

	// Create the transaction used to return the current settled balance
	// on this active channel back to both parties. In this current model,
	// the initiator pays full fees for the cooperative close transaction.
	closeTx := createCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		false)

	// With the transaction created, we can finally generate our half of
	// the 2-of-2 multi-sig needed to redeem the funding output.
	redeemScript := lc.channelState.FundingRedeemScript
	hashCache := txscript.NewTxSigHashes(closeTx)
	closeSig, err := txscript.RawTxInWitnessSignature(closeTx,
		hashCache, 0, int64(lc.channelState.Capacity),
		redeemScript, txscript.SigHashAll,
		lc.channelState.OurMultiSigKey)
	if err != nil {
		return nil, err
	}

	// Finally, construct the witness stack minding the order of the
	// pubkeys+sigs on the stack.
	ourKey := lc.channelState.OurMultiSigKey.PubKey().SerializeCompressed()
	theirKey := lc.channelState.TheirMultiSigKey.SerializeCompressed()
	witness := spendMultiSig(redeemScript, ourKey, closeSig,
		theirKey, remoteSig)
	closeTx.TxIn[0].Witness = witness

	// TODO(roasbeef): VALIDATE

	return closeTx, nil
}

// DeleteState deletes all state concerning the channel from the underlying
// database, only leaving a small summary describing meta-data of the
// channel's lifetime.
func (lc *LightningChannel) DeleteState() error {
	return lc.channelState.CloseChannel()
}

// StateSnapshot returns a snapshot b
func (lc *LightningChannel) StateSnapshot() *channeldb.ChannelSnapshot {
	lc.stateMtx.RLock()
	defer lc.stateMtx.RUnlock()

	return lc.channelState.Snapshot()
}

// RequestPayment...
func (lc *LightningChannel) RequestPayment(amount btcutil.Amount) error {
	// Validate amount
	return nil
}

// PaymentRequest...
// TODO(roasbeef): serialization (bip 70, QR code, etc)
//  * routing handled by upper layer
type PaymentRequest struct {
	PaymentPreImage [20]byte
	Value           btcutil.Amount
}

// createNewCommitmentTxns....
// NOTE: This MUST be called with stateMtx held.
func createNewCommitmentTxns(fundingTxIn *wire.TxIn, state *channeldb.OpenChannel,
	chanUpdate *ChannelUpdate, amountToUs, amountToThem btcutil.Amount) (*wire.MsgTx, *wire.MsgTx, error) {

	ourNewCommitTx, err := createCommitTx(fundingTxIn,
		state.OurCommitKey.PubKey(), state.TheirCommitKey,
		chanUpdate.pendingDesc.OurRevocation[:], state.LocalCsvDelay,
		amountToUs, amountToThem)
	if err != nil {
		return nil, nil, err
	}

	theirNewCommitTx, err := createCommitTx(fundingTxIn,
		state.TheirCommitKey, state.OurCommitKey.PubKey(),
		chanUpdate.pendingDesc.TheirRevocation[:], state.RemoteCsvDelay,
		amountToThem, amountToUs)
	if err != nil {
		return nil, nil, err
	}

	return ourNewCommitTx, theirNewCommitTx, nil
}

// createCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one paying
// to the "owner" of the commitment transaction which can be spent after a
// relative block delay or revocation event, and the other paying the the
// counter-party within the channel, which can be spent immediately.
func createCommitTx(fundingOutput *wire.TxIn, selfKey, theirKey *btcec.PublicKey,
	revokeHash []byte, csvTimeout uint32, amountToSelf,
	amountToThem btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	revokeKey := deriveRevocationPubkey(theirKey, revokeHash)
	ourRedeemScript, err := commitScriptToSelf(csvTimeout, selfKey,
		revokeKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2WKH output, without any added CSV delay.
	theirWitnessKeyHash, err := commitScriptUnencumbered(theirKey)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx()
	commitTx.Version = 2
	commitTx.AddTxIn(fundingOutput)
	commitTx.AddTxOut(wire.NewTxOut(int64(amountToSelf), payToUsScriptHash))
	commitTx.AddTxOut(wire.NewTxOut(int64(amountToThem), theirWitnessKeyHash))

	return commitTx, nil
}

// createCooperativeCloseTx creates a transaction which if signed by both
// parties, then broadcast cooperatively closes an active channel. The creation
// of the closure transaction is modified by a boolean indicating if the party
// constructing the channel is the initiator of the closure. Currently it is
// expected that the initiator pays the transaction fees for the closing
// transaction in full.
func createCooperativeCloseTx(fundingTxIn *wire.TxIn,
	ourBalance, theirBalance btcutil.Amount,
	ourDeliveryScript, theirDeliveryScript []byte,
	initiator bool) *wire.MsgTx {

	// Construct the transaction to perform a cooperative closure of the
	// channel. In the event that one side doesn't have any settled funds
	// within the channel then a refund output for that particular side can
	// be omitted.
	closeTx := wire.NewMsgTx()
	closeTx.AddTxIn(fundingTxIn)

	// The initiator the a cooperative closure pays the fee in entirety.
	// Determine if we're the initiator so we can compute fees properly.
	if initiator {
		// TODO(roasbeef): take sat/byte here instead of properly calc
		ourBalance -= 5000
	} else {
		theirBalance -= 5000
	}

	// TODO(roasbeef): dust check...
	//  * although upper layers should prevent
	if ourBalance != 0 {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: ourDeliveryScript,
			Value:    int64(ourBalance),
		})
	}
	if theirBalance != 0 {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: theirDeliveryScript,
			Value:    int64(theirBalance),
		})
	}

	txsort.InPlaceSort(closeTx)

	return closeTx
}
