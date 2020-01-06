package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CommitmentKeyRing holds all derived keys needed to construct commitment and
// HTLC transactions. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's. Private keys associated
// with each key may belong to the commitment owner or the "other party" which
// is referred to in the field comments, regardless of which is local and which
// is remote.
type CommitmentKeyRing struct {
	// commitPoint is the "per commitment point" used to derive the tweak
	// for each base point.
	CommitPoint *btcec.PublicKey

	// LocalCommitKeyTweak is the tweak used to derive the local public key
	// from the local payment base point or the local private key from the
	// base point secret. This may be included in a SignDescriptor to
	// generate signatures for the local payment key.
	LocalCommitKeyTweak []byte

	// TODO(roasbeef): need delay tweak as well?

	// LocalHtlcKeyTweak is the teak used to derive the local HTLC key from
	// the local HTLC base point. This value is needed in order to
	// derive the final key used within the HTLC scripts in the commitment
	// transaction.
	LocalHtlcKeyTweak []byte

	// LocalHtlcKey is the key that will be used in the "to self" clause of
	// any HTLC scripts within the commitment transaction for this key ring
	// set.
	LocalHtlcKey *btcec.PublicKey

	// RemoteHtlcKey is the key that will be used in clauses within the
	// HTLC script that send money to the remote party.
	RemoteHtlcKey *btcec.PublicKey

	// DelayKey is the commitment transaction owner's key which is included
	// in HTLC success and timeout transaction scripts.
	DelayKey *btcec.PublicKey

	// NoDelayKey is the other party's payment key in the commitment tx.
	// This is the key used to generate the unencumbered output within the
	// commitment transaction.
	NoDelayKey *btcec.PublicKey

	// RevocationKey is the key that can be used by the other party to
	// redeem outputs from a revoked commitment transaction if it were to
	// be published.
	RevocationKey *btcec.PublicKey
}

// DeriveCommitmentKey generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's.
func DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit, tweaklessCommit bool,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *CommitmentKeyRing {

	// First, we'll derive all the keys that don't depend on the context of
	// whose commitment transaction this is.
	keyRing := &CommitmentKeyRing{
		CommitPoint: commitPoint,

		LocalCommitKeyTweak: input.SingleTweakBytes(
			commitPoint, localChanCfg.PaymentBasePoint.PubKey,
		),
		LocalHtlcKeyTweak: input.SingleTweakBytes(
			commitPoint, localChanCfg.HtlcBasePoint.PubKey,
		),
		LocalHtlcKey: input.TweakPubKey(
			localChanCfg.HtlcBasePoint.PubKey, commitPoint,
		),
		RemoteHtlcKey: input.TweakPubKey(
			remoteChanCfg.HtlcBasePoint.PubKey, commitPoint,
		),
	}

	// We'll now compute the delay, no delay, and revocation key based on
	// the current commitment point. All keys are tweaked each state in
	// order to ensure the keys from each state are unlinkable. To create
	// the revocation key, we take the opposite party's revocation base
	// point and combine that with the current commitment point.
	var (
		delayBasePoint      *btcec.PublicKey
		noDelayBasePoint    *btcec.PublicKey
		revocationBasePoint *btcec.PublicKey
	)
	if isOurCommit {
		delayBasePoint = localChanCfg.DelayBasePoint.PubKey
		noDelayBasePoint = remoteChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = remoteChanCfg.RevocationBasePoint.PubKey
	} else {
		delayBasePoint = remoteChanCfg.DelayBasePoint.PubKey
		noDelayBasePoint = localChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = localChanCfg.RevocationBasePoint.PubKey
	}

	// With the base points assigned, we can now derive the actual keys
	// using the base point, and the current commitment tweak.
	keyRing.DelayKey = input.TweakPubKey(delayBasePoint, commitPoint)
	keyRing.RevocationKey = input.DeriveRevocationPubkey(
		revocationBasePoint, commitPoint,
	)

	// If this commitment should omit the tweak for the remote point, then
	// we'll use that directly, and ignore the commitPoint tweak.
	if tweaklessCommit {
		keyRing.NoDelayKey = noDelayBasePoint
	} else {
		keyRing.NoDelayKey = input.TweakPubKey(
			noDelayBasePoint, commitPoint,
		)
	}

	return keyRing
}

// createStateHintObfuscator derives and assigns the state hint obfuscator for
// the channel, which is used to encode the commitment height in the sequence
// number of commitment transaction inputs.
func (lc *LightningChannel) createStateHintObfuscator() {
	state := lc.channelState
	if state.IsInitiator {
		lc.stateHintObfuscator = DeriveStateHintObfuscator(
			state.LocalChanCfg.PaymentBasePoint.PubKey,
			state.RemoteChanCfg.PaymentBasePoint.PubKey,
		)
	} else {
		lc.stateHintObfuscator = DeriveStateHintObfuscator(
			state.RemoteChanCfg.PaymentBasePoint.PubKey,
			state.LocalChanCfg.PaymentBasePoint.PubKey,
		)
	}
}

// createCommitmentTx generates the unsigned commitment transaction for a
// commitment view and assigns to txn field.
func (lc *LightningChannel) createCommitmentTx(c *commitment,
	filteredHTLCView *htlcView, keyRing *CommitmentKeyRing) error {

	ourBalance := c.ourBalance
	theirBalance := c.theirBalance

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := input.CommitWeight + (input.HtlcWeight * numHTLCs)

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := c.feePerKw.FeeForWeight(totalCommitWeight)
	commitFeeMSat := lnwire.NewMSatFromSatoshis(commitFee)

	// Currently, within the protocol, the initiator always pays the fees.
	// So we'll subtract the fee amount from the balance of the current
	// initiator. If the initiator is unable to pay the fee fully, then
	// their entire output is consumed.
	switch {
	case lc.channelState.IsInitiator && commitFee > ourBalance.ToSatoshis():
		ourBalance = 0

	case lc.channelState.IsInitiator:
		ourBalance -= commitFeeMSat

	case !lc.channelState.IsInitiator && commitFee > theirBalance.ToSatoshis():
		theirBalance = 0

	case !lc.channelState.IsInitiator:
		theirBalance -= commitFeeMSat
	}

	var (
		commitTx *wire.MsgTx
		err      error
	)

	// Depending on whether the transaction is ours or not, we call
	// CreateCommitTx with parameters mathcing the perspective, to generate
	// a new commitment transaction with all the latest unsettled/un-timed
	// out HTLCs.
	if c.isOurs {
		commitTx, err = CreateCommitTx(
			fundingTxIn(lc.channelState), keyRing, &lc.channelState.LocalChanCfg,
			&lc.channelState.RemoteChanCfg, ourBalance.ToSatoshis(),
			theirBalance.ToSatoshis(),
		)
	} else {
		commitTx, err = CreateCommitTx(
			fundingTxIn(lc.channelState), keyRing, &lc.channelState.RemoteChanCfg,
			&lc.channelState.LocalChanCfg, theirBalance.ToSatoshis(),
			ourBalance.ToSatoshis(),
		)
	}
	if err != nil {
		return err
	}

	// We'll now add all the HTLC outputs to the commitment transaction.
	// Each output includes an off-chain 2-of-2 covenant clause, so we'll
	// need the objective local/remote keys for this particular commitment
	// as well. For any non-dust HTLCs that are manifested on the commitment
	// transaction, we'll also record its CLTV which is required to sort the
	// commitment transaction below. The slice is initially sized to the
	// number of existing outputs, since any outputs already added are
	// commitment outputs and should correspond to zero values for the
	// purposes of sorting.
	cltvs := make([]uint32, len(commitTx.TxOut))
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, false, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, true, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, c.height, lc.stateHintObfuscator)
	if err != nil {
		return err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	InPlaceCommitSort(commitTx, cltvs)

	// Next, we'll ensure that we don't accidentally create a commitment
	// transaction which would be invalid by consensus.
	uTx := btcutil.NewTx(commitTx)
	if err := blockchain.CheckTransactionSanity(uTx); err != nil {
		return err
	}

	// Finally, we'll assert that were not attempting to draw more out of
	// the channel that was originally placed within it.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	if totalOut > lc.channelState.Capacity {
		return fmt.Errorf("height=%v, for ChannelPoint(%v) attempts "+
			"to consume %v while channel capacity is %v",
			c.height, lc.channelState.FundingOutpoint,
			totalOut, lc.channelState.Capacity)
	}

	c.txn = commitTx
	c.fee = commitFee
	c.ourBalance = ourBalance
	c.theirBalance = theirBalance
	return nil
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one local
// output paying to the "owner" of the commitment transaction which can be
// spent after a relative block delay or revocation event, and a remote output
// paying the counterparty within the channel, which can be spent immediately
// or after a delay depending on the commitment type..
func CreateCommitTx(fundingOutput wire.TxIn, keyRing *CommitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	toLocalRedeemScript, err := input.CommitScriptToSelf(
		uint32(localChanCfg.CsvDelay), keyRing.DelayKey,
		keyRing.RevocationKey,
	)
	if err != nil {
		return nil, err
	}
	toLocalScriptHash, err := input.WitnessScriptHash(
		toLocalRedeemScript,
	)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to the remote. This is just a
	// regular P2WPKH output, without any added CSV delay.
	toRemoteWitnessKeyHash, err := input.CommitScriptUnencumbered(
		keyRing.NoDelayKey,
	)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&fundingOutput)

	// Avoid creating dust outputs within the commitment transaction.
	if amountToLocal >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toLocalScriptHash,
			Value:    int64(amountToLocal),
		})
	}
	if amountToRemote >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toRemoteWitnessKeyHash,
			Value:    int64(amountToRemote),
		})
	}

	return commitTx, nil
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours.
func genHtlcScript(isIncoming, ourCommit bool, timeout uint32, rHash [32]byte,
	keyRing *CommitmentKeyRing) ([]byte, []byte, error) {

	var (
		witnessScript []byte
		err           error
	)

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(timeout,
			keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:])

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		witnessScript, err = input.SenderHTLCScript(keyRing.RemoteHtlcKey,
			keyRing.LocalHtlcKey, keyRing.RevocationKey, rHash[:])

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		witnessScript, err = input.SenderHTLCScript(keyRing.LocalHtlcKey,
			keyRing.RemoteHtlcKey, keyRing.RevocationKey, rHash[:])

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(timeout, keyRing.LocalHtlcKey,
			keyRing.RemoteHtlcKey, keyRing.RevocationKey, rHash[:])
	}
	if err != nil {
		return nil, nil, err
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, nil, err
	}

	return htlcP2WSH, witnessScript, nil
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// PaymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	isIncoming bool, paymentDesc *PaymentDescriptor,
	keyRing *CommitmentKeyRing) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	p2wsh, witnessScript, err := genHtlcScript(isIncoming, ourCommit,
		timeout, rHash, keyRing)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount.ToSatoshis())
	commitTx.AddTxOut(wire.NewTxOut(amountPending, p2wsh))

	// Store the pkScript of this particular PaymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if ourCommit {
		paymentDesc.ourPkScript = p2wsh
		paymentDesc.ourWitnessScript = witnessScript
	} else {
		paymentDesc.theirPkScript = p2wsh
		paymentDesc.theirWitnessScript = witnessScript
	}

	return nil
}
