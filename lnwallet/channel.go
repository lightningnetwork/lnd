package lnwallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/channeldb"

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

type PaymentHash [20]byte

// LightningChannel...
// TODO(roasbeef): future peer struct should embed this struct
type LightningChannel struct {
	lnwallet      *LightningWallet
	channelEvents *chainntnfs.ChainNotifier

	// TODO(roasbeef): Stores all previous R values + timeouts for each
	// commitment update, plus some other meta-data...Or just use OP_RETURN
	// to help out?
	// currently going for: nSequence/nLockTime overloading
	channelDB *channeldb.DB

	// stateMtx protects concurrent access to the state struct.
	stateMtx     sync.RWMutex
	channelState channeldb.OpenChannel

	// Payment's which we've requested.
	unfufilledPayments map[PaymentHash]*PaymentRequest

	// Uncleared HTLC's.
	pendingPayments map[PaymentHash]*PaymentDescriptor

	ourPendingCommitTx   *wire.MsgTx
	theirPendingCommitTx *wire.MsgTx

	fundingTxIn *wire.TxIn

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// newLightningChannel...
func newLightningChannel(wallet *LightningWallet, events *chainntnfs.ChainNotifier,
	chanDB *channeldb.DB, state channeldb.OpenChannel) (*LightningChannel, error) {

	lc := &LightningChannel{
		lnwallet:      wallet,
		channelEvents: events,
		channelDB:     chanDB,
		channelState:  state,
	}

	fundingTxId := state.FundingTx.TxSha()
	fundingPkScript, err := scriptHashPkScript(state.FundingRedeemScript)
	if err != nil {
		return nil, err
	}
	_, multiSigIndex := findScriptOutputIndex(state.FundingTx, fundingPkScript)
	lc.fundingTxIn = wire.NewTxIn(wire.NewOutPoint(&fundingTxId, multiSigIndex), nil)

	return lc, nil
}

// PaymentDescriptor...
type PaymentDescriptor struct {
	RHash           [20]byte
	OurRevocation   [20]byte // TODO(roasbeef): don't need these?
	TheirRevocation [20]byte

	Timeout uint32
	Value   btcutil.Amount
	PayToUs bool

	lnchannel *LightningChannel
}

// AddHTLC...
// 1. request R_Hash from receiver (only if single hop, would be out of band)
// 2. propose HTLC
//    * timeout
//    * value
//    * r_hash
//    * next revocation hash
// Can build our new commitment tx at this point
// 3. they accept
//    * their next revocation hash
//    * their sig for our new commitment tx (verify correctness)
// 4. we give sigs
//    * our sigs for their new commitment tx
//    * the pre-image to our old commitment tx
// 5. they complete
//    * the pre-image to their old commitment tx (verify is part of their chain, is pre-image)
func (lc *LightningChannel) AddHTLC(timeout uint32, value btcutil.Amount,
	rHash, revocation PaymentHash, payToUs bool) (*PaymentDescriptor, []byte, error) {

	lc.stateMtx.Lock()
	defer lc.stateMtx.Unlock()

	paymentDetails := &PaymentDescriptor{
		RHash:           rHash,
		TheirRevocation: revocation,
		Timeout:         timeout,
		Value:           value,
		PayToUs:         payToUs,
		lnchannel:       lc,
	}

	lc.channelState.TheirCurrentRevocation = revocation

	// Get next revocation hash, updating the number of updates in the
	// channel as a result.
	updateNum := lc.channelState.NumUpdates + 1
	nextPreimage, err := lc.channelState.OurShaChain.GetHash(updateNum)
	if err != nil {
		return nil, nil, err
	}
	copy(paymentDetails.OurRevocation[:], btcutil.Hash160(nextPreimage[:]))
	lc.channelState.NumUpdates = updateNum

	// Re-calculate the amount of cleared funds for each side.
	var amountToUs, amountToThem btcutil.Amount
	if payToUs {
		amountToUs = lc.channelState.OurBalance + value
		amountToThem = lc.channelState.TheirBalance - value
	} else {
		amountToUs = lc.channelState.OurBalance - value
		amountToThem = lc.channelState.TheirBalance + value
	}

	// Re-create copies of the current commitment transactions to be updated.
	ourNewCommitTx, err := createCommitTx(lc.fundingTxIn,
		lc.channelState.OurCommitKey.PubKey(), lc.channelState.TheirCommitKey,
		paymentDetails.OurRevocation[:], lc.channelState.CsvDelay,
		amountToUs, amountToThem)
	if err != nil {
		return nil, nil, err
	}
	theirNewCommitTx, err := createCommitTx(lc.fundingTxIn,
		lc.channelState.TheirCommitKey, lc.channelState.OurCommitKey.PubKey(),
		paymentDetails.TheirRevocation[:], lc.channelState.CsvDelay,
		amountToThem, amountToUs)
	if err != nil {
		return nil, nil, err
	}

	// First, re-add all the old HTLCs.
	for _, paymentDesc := range lc.pendingPayments {
		if err := lc.addHTLC(ourNewCommitTx, theirNewCommitTx, paymentDesc); err != nil {
			return nil, nil, err
		}
	}

	// Then add this new HTLC.
	if err := lc.addHTLC(ourNewCommitTx, theirNewCommitTx, paymentDetails); err != nil {
		return nil, nil, err
	}
	lc.pendingPayments[rHash] = paymentDetails // TODO(roasbeef): check for dups?

	// Sort both transactions according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourNewCommitTx)
	txsort.InPlaceSort(theirNewCommitTx)

	// TODO(roasbeef): locktimes/sequence set

	// Sign their version of the commitment transaction.
	sigTheirCommit, err := txscript.RawTxInSignature(theirNewCommitTx, 0,
		lc.channelState.FundingRedeemScript, txscript.SigHashAll,
		lc.channelState.MultiSigKey)
	if err != nil {
		return nil, nil, err
	}

	// TODO(roasbeef): write checkpoint here...
	return paymentDetails, sigTheirCommit, nil
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
	delay := lc.channelState.CsvDelay
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
func (lc *LightningChannel) SettleHTLC(rValue []byte) error {
	return nil
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
	// the transaction itself.
	commitTx := wire.NewMsgTx()
	commitTx.AddTxIn(fundingOutput)
	// TODO(roasbeef): we default to blocks, make configurable as part of
	// channel reservation.
	commitTx.TxIn[0].Sequence = lockTimeToSequence(false, csvTimeout)
	commitTx.AddTxOut(wire.NewTxOut(int64(amountToSelf), payToUsScriptHash))
	commitTx.AddTxOut(wire.NewTxOut(int64(amountToThem), payToThemScriptHash))

	return commitTx, nil
}

// lockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//  * (Compatibility)
func lockTimeToSequence(isSeconds bool, locktime uint32) uint32 {
	if !isSeconds {
		// The locktime is to be expressed in confirmations. Apply the
		// mask to restrict the number of confirmations to 65,535 or
		// 1.25 years.
		return SequenceLockTimeMask & locktime
	}

	// Set the 22nd bit which indicates the lock time is in seconds, then
	// shift the locktime over by 9 since the time granularity is in
	// 512-second intervals (2^9). This results in a max lock-time of
	// 33,554,431 seconds, or 1.06 years.
	return SequenceLockTimeSeconds | (locktime >> 9)
}
