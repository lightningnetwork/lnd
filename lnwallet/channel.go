package lnwallet

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/chainntfs"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/txsort"
)

const (
	// TODO(roasbeef): make not random value
	MaxPendingPayments = 10
)

// PaymentHash presents the hash160 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [20]byte

// LightningChannel...
// TODO(roasbeef): future peer struct should embed this struct
type LightningChannel struct {
	lnwallet      *LightningWallet
	channelEvents chainntnfs.ChainNotifier

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

// newLightningChannel...
func newLightningChannel(wallet *LightningWallet, events chainntnfs.ChainNotifier,
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

	fundingTxId := state.FundingTx.TxSha()
	fundingPkScript, err := scriptHashPkScript(state.FundingRedeemScript)
	if err != nil {
		return nil, err
	}
	_, multiSigIndex := findScriptOutputIndex(state.FundingTx, fundingPkScript)
	lc.fundingTxIn = wire.NewTxIn(wire.NewOutPoint(&fundingTxId, multiSigIndex), nil, nil)
	lc.fundingP2SH = fundingPkScript

	return lc, nil
}

// PaymentDescriptor...
type PaymentDescriptor struct {
	RHash   [20]byte
	Timeout uint32
	Value   btcutil.Amount

	OurRevocation   [20]byte // TODO(roasbeef): don't need these?
	TheirRevocation [20]byte

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

	pendingRevocation [20]byte
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

	return btcutil.Hash160(nextPreimage[:]), nil
}

// SignCounterPartyCommitment...
func (c *ChannelUpdate) SignCounterPartyCommitment() ([]byte, error) {
	c.lnChannel.stateMtx.RLock()
	defer c.lnChannel.stateMtx.RUnlock()

	if c.sigTheirNewCommit != nil {
		return c.sigTheirNewCommit, nil
	}

	// Sign their version of the commitment transaction.
	sig, err := txscript.RawTxInSignature(c.theirPendingCommitTx, 0,
		c.lnChannel.channelState.FundingRedeemScript, txscript.SigHashAll,
		c.lnChannel.channelState.MultiSigKey)
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

	var err error
	var scriptSig []byte
	channelState := c.lnChannel.channelState

	// When initially generating the redeemScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virual Machine stack in
	// the correct order.
	redeemScript := channelState.FundingRedeemScript
	ourKey := channelState.OurCommitKey.PubKey().SerializeCompressed()
	theirKey := channelState.TheirCommitKey.SerializeCompressed()
	scriptSig, err = spendMultiSig(redeemScript, ourKey, ourSig, theirKey, theirSig)
	if err != nil {
		return err
	}

	// Attach the scriptSig to our commitment transaction's only input,
	// then validate that the scriptSig executes correctly.
	commitTx := c.ourPendingCommitTx
	commitTx.TxIn[0].SignatureScript = scriptSig
	vm, err := txscript.NewEngine(c.lnChannel.fundingP2SH, commitTx, 0,
		txscript.StandardVerifyFlags, nil)
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

	// Now that we have the redeem scripts, create the P2SH public key
	// script for each.
	senderP2SH, err := scriptHashPkScript(senderPKScript)
	if err != nil {
		return nil
	}
	receiverP2SH, err := scriptHashPkScript(receiverPKScript)
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
func (lc *LightningChannel) SettleHTLC(rValue [20]byte, newRevocation [20]byte) (*ChannelUpdate, error) {
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

// CancelHTLC...
func (lc *LightningChannel) CancelHTLC() error {
	return nil
}

// OurBalance...
func (lc *LightningChannel) OurBalance() btcutil.Amount {
	lc.stateMtx.RLock()
	defer lc.stateMtx.RUnlock()
	return lc.channelState.OurBalance
}

// TheirBalance...
func (lc *LightningChannel) TheirBalance() btcutil.Amount {
	lc.stateMtx.RLock()
	defer lc.stateMtx.RUnlock()
	return lc.channelState.TheirBalance
}

// ForceClose...
func (lc *LightningChannel) ForceClose() error {
	return nil
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

// createCommitTx...
// TODO(roasbeef): fix inconsistency of 32 vs 20 byte revocation hashes everywhere...
func createCommitTx(fundingOutput *wire.TxIn, selfKey, theirKey *btcec.PublicKey,
	revokeHash []byte, csvTimeout uint32, amountToSelf,
	amountToThem btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	ourRedeemScript, err := commitScriptToSelf(csvTimeout, selfKey, theirKey,
		revokeHash)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := scriptHashPkScript(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2PKH-like output, without any added CSV delay. However, we instead
	// use P2SH.
	theirRedeemScript, err := commitScriptUnencumbered(theirKey)
	if err != nil {
		return nil, err
	}
	payToThemScriptHash, err := scriptHashPkScript(theirRedeemScript)
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
	commitTx.AddTxOut(wire.NewTxOut(int64(amountToThem), payToThemScriptHash))

	return commitTx, nil
}
