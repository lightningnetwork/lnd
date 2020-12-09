package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// anchorSize is the constant anchor output size.
const anchorSize = btcutil.Amount(330)

// CommitmentKeyRing holds all derived keys needed to construct commitment and
// HTLC transactions. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's. Private keys associated
// with each key may belong to the commitment owner or the "other party" which
// is referred to in the field comments, regardless of which is local and which
// is remote.
type CommitmentKeyRing struct {
	// CommitPoint is the "per commitment point" used to derive the tweak
	// for each base point.
	CommitPoint *btcec.PublicKey

	// LocalCommitKeyTweak is the tweak used to derive the local public key
	// from the local payment base point or the local private key from the
	// base point secret. This may be included in a SignDescriptor to
	// generate signatures for the local payment key.
	//
	// NOTE: This will always refer to "our" local key, regardless of
	// whether this is our commit or not.
	LocalCommitKeyTweak []byte

	// TODO(roasbeef): need delay tweak as well?

	// LocalHtlcKeyTweak is the tweak used to derive the local HTLC key
	// from the local HTLC base point. This value is needed in order to
	// derive the final key used within the HTLC scripts in the commitment
	// transaction.
	//
	// NOTE: This will always refer to "our" local HTLC key, regardless of
	// whether this is our commit or not.
	LocalHtlcKeyTweak []byte

	// LocalHtlcKey is the key that will be used in any clause paying to
	// our node of any HTLC scripts within the commitment transaction for
	// this key ring set.
	//
	// NOTE: This will always refer to "our" local HTLC key, regardless of
	// whether this is our commit or not.
	LocalHtlcKey *btcec.PublicKey

	// RemoteHtlcKey is the key that will be used in clauses within the
	// HTLC script that send money to the remote party.
	//
	// NOTE: This will always refer to "their" remote HTLC key, regardless
	// of whether this is our commit or not.
	RemoteHtlcKey *btcec.PublicKey

	// ToLocalKey is the commitment transaction owner's key which is
	// included in HTLC success and timeout transaction scripts. This is
	// the public key used for the to_local output of the commitment
	// transaction.
	//
	// NOTE: Who's key this is depends on the current perspective. If this
	// is our commitment this will be our key.
	ToLocalKey *btcec.PublicKey

	// ToRemoteKey is the non-owner's payment key in the commitment tx.
	// This is the key used to generate the to_remote output within the
	// commitment transaction.
	//
	// NOTE: Who's key this is depends on the current perspective. If this
	// is our commitment this will be their key.
	ToRemoteKey *btcec.PublicKey

	// RevocationKey is the key that can be used by the other party to
	// redeem outputs from a revoked commitment transaction if it were to
	// be published.
	//
	// NOTE: Who can sign for this key depends on the current perspective.
	// If this is our commitment, it means the remote node can sign for
	// this key in case of a breach.
	RevocationKey *btcec.PublicKey
}

// DeriveCommitmentKeys generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending on the type
// of channel, and whether the commitment transaction is ours or the remote
// peer's.
func DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, chanType channeldb.ChannelType,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *CommitmentKeyRing {

	tweaklessCommit := chanType.IsTweakless()

	// Depending on if this is our commit or not, we'll choose the correct
	// base point.
	localBasePoint := localChanCfg.PaymentBasePoint
	if isOurCommit {
		localBasePoint = localChanCfg.DelayBasePoint
	}

	// First, we'll derive all the keys that don't depend on the context of
	// whose commitment transaction this is.
	keyRing := &CommitmentKeyRing{
		CommitPoint: commitPoint,

		LocalCommitKeyTweak: input.SingleTweakBytes(
			commitPoint, localBasePoint.PubKey,
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

	// We'll now compute the to_local, to_remote, and revocation key based
	// on the current commitment point. All keys are tweaked each state in
	// order to ensure the keys from each state are unlinkable. To create
	// the revocation key, we take the opposite party's revocation base
	// point and combine that with the current commitment point.
	var (
		toLocalBasePoint    *btcec.PublicKey
		toRemoteBasePoint   *btcec.PublicKey
		revocationBasePoint *btcec.PublicKey
	)
	if isOurCommit {
		toLocalBasePoint = localChanCfg.DelayBasePoint.PubKey
		toRemoteBasePoint = remoteChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = remoteChanCfg.RevocationBasePoint.PubKey
	} else {
		toLocalBasePoint = remoteChanCfg.DelayBasePoint.PubKey
		toRemoteBasePoint = localChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = localChanCfg.RevocationBasePoint.PubKey
	}

	// With the base points assigned, we can now derive the actual keys
	// using the base point, and the current commitment tweak.
	keyRing.ToLocalKey = input.TweakPubKey(toLocalBasePoint, commitPoint)
	keyRing.RevocationKey = input.DeriveRevocationPubkey(
		revocationBasePoint, commitPoint,
	)

	// If this commitment should omit the tweak for the remote point, then
	// we'll use that directly, and ignore the commitPoint tweak.
	if tweaklessCommit {
		keyRing.ToRemoteKey = toRemoteBasePoint

		// If this is not our commitment, the above ToRemoteKey will be
		// ours, and we blank out the local commitment tweak to
		// indicate that the key should not be tweaked when signing.
		if !isOurCommit {
			keyRing.LocalCommitKeyTweak = nil
		}
	} else {
		keyRing.ToRemoteKey = input.TweakPubKey(
			toRemoteBasePoint, commitPoint,
		)
	}

	return keyRing
}

// ScriptInfo holds a redeem script and hash.
type ScriptInfo struct {
	// PkScript is the output's PkScript.
	PkScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output. This field should be set to the full script if a p2wsh
	// output is being signed. For p2wkh it should be set equal to the
	// PkScript.
	WitnessScript []byte
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// channel type. The second return value is the CSV deleay of the output
// script, what must be satisfied in order to spend the output.
func CommitScriptToRemote(chanType channeldb.ChannelType,
	key *btcec.PublicKey) (*ScriptInfo, uint32, error) {

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	if chanType.HasAnchors() {
		script, err := input.CommitScriptToRemoteConfirmed(key)
		if err != nil {
			return nil, 0, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, 0, err
		}

		return &ScriptInfo{
			PkScript:      p2wsh,
			WitnessScript: script,
		}, 1, nil
	}

	// Otherwise the to_remote will be a simple p2wkh.
	p2wkh, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, 0, err
	}

	// Since this is a regular P2WKH, the WitnessScipt and PkScript should
	// both be set to the script hash.
	return &ScriptInfo{
		WitnessScript: p2wkh,
		PkScript:      p2wkh,
	}, 0, nil
}

// HtlcSigHashType returns the sighash type to use for HTLC success and timeout
// transactions given the channel type.
func HtlcSigHashType(chanType channeldb.ChannelType) txscript.SigHashType {
	if chanType.HasAnchors() {
		return txscript.SigHashSingle | txscript.SigHashAnyOneCanPay
	}

	return txscript.SigHashAll
}

// HtlcSignDetails converts the passed parameters to a SignDetails valid for
// this channel type. For non-anchor channels this will return nil.
func HtlcSignDetails(chanType channeldb.ChannelType, signDesc input.SignDescriptor,
	sigHash txscript.SigHashType, peerSig input.Signature) *input.SignDetails {

	// Non-anchor channels don't need sign details, as the HTLC second
	// level cannot be altered.
	if !chanType.HasAnchors() {
		return nil
	}

	return &input.SignDetails{
		SignDesc:    signDesc,
		SigHashType: sigHash,
		PeerSig:     peerSig,
	}
}

// HtlcSecondLevelInputSequence dictates the sequence number we must use on the
// input to a second level HTLC transaction.
func HtlcSecondLevelInputSequence(chanType channeldb.ChannelType) uint32 {
	if chanType.HasAnchors() {
		return 1
	}

	return 0
}

// CommitWeight returns the base commitment weight before adding HTLCs.
func CommitWeight(chanType channeldb.ChannelType) int64 {
	// If this commitment has anchors, it will be slightly heavier.
	if chanType.HasAnchors() {
		return input.AnchorCommitWeight
	}

	return input.CommitWeight
}

// HtlcTimeoutFee returns the fee in satoshis required for an HTLC timeout
// transaction based on the current fee rate.
func HtlcTimeoutFee(chanType channeldb.ChannelType,
	feePerKw chainfee.SatPerKWeight) btcutil.Amount {

	if chanType.HasAnchors() {
		return feePerKw.FeeForWeight(input.HtlcTimeoutWeightConfirmed)
	}

	return feePerKw.FeeForWeight(input.HtlcTimeoutWeight)
}

// HtlcSuccessFee returns the fee in satoshis required for an HTLC success
// transaction based on the current fee rate.
func HtlcSuccessFee(chanType channeldb.ChannelType,
	feePerKw chainfee.SatPerKWeight) btcutil.Amount {

	if chanType.HasAnchors() {
		return feePerKw.FeeForWeight(input.HtlcSuccessWeightConfirmed)
	}
	return feePerKw.FeeForWeight(input.HtlcSuccessWeight)
}

// CommitScriptAnchors return the scripts to use for the local and remote
// anchor.
func CommitScriptAnchors(localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) (*ScriptInfo,
	*ScriptInfo, error) {

	// Helper to create anchor ScriptInfo from key.
	anchorScript := func(key *btcec.PublicKey) (*ScriptInfo, error) {
		script, err := input.CommitScriptAnchor(key)
		if err != nil {
			return nil, err
		}

		scriptHash, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, err
		}

		return &ScriptInfo{
			PkScript:      scriptHash,
			WitnessScript: script,
		}, nil
	}

	// Get the script used for the anchor output spendable by the local
	// node.
	localAnchor, err := anchorScript(localChanCfg.MultiSigKey.PubKey)
	if err != nil {
		return nil, nil, err
	}

	// And the anchor spendable by the remote node.
	remoteAnchor, err := anchorScript(remoteChanCfg.MultiSigKey.PubKey)
	if err != nil {
		return nil, nil, err
	}

	return localAnchor, remoteAnchor, nil
}

// CommitmentBuilder is a type that wraps the type of channel we are dealing
// with, and abstracts the various ways of constructing commitment
// transactions.
type CommitmentBuilder struct {
	// chanState is the underlying channels's state struct, used to
	// determine the type of channel we are dealing with, and relevant
	// parameters.
	chanState *channeldb.OpenChannel

	// obfuscator is a 48-bit state hint that's used to obfuscate the
	// current state number on the commitment transactions.
	obfuscator [StateHintSize]byte
}

// NewCommitmentBuilder creates a new CommitmentBuilder from chanState.
func NewCommitmentBuilder(chanState *channeldb.OpenChannel) *CommitmentBuilder {
	// The anchor channel type MUST be tweakless.
	if chanState.ChanType.HasAnchors() && !chanState.ChanType.IsTweakless() {
		panic("invalid channel type combination")
	}

	return &CommitmentBuilder{
		chanState:  chanState,
		obfuscator: createStateHintObfuscator(chanState),
	}
}

// createStateHintObfuscator derives and assigns the state hint obfuscator for
// the channel, which is used to encode the commitment height in the sequence
// number of commitment transaction inputs.
func createStateHintObfuscator(state *channeldb.OpenChannel) [StateHintSize]byte {
	if state.IsInitiator {
		return DeriveStateHintObfuscator(
			state.LocalChanCfg.PaymentBasePoint.PubKey,
			state.RemoteChanCfg.PaymentBasePoint.PubKey,
		)
	}

	return DeriveStateHintObfuscator(
		state.RemoteChanCfg.PaymentBasePoint.PubKey,
		state.LocalChanCfg.PaymentBasePoint.PubKey,
	)
}

// unsignedCommitmentTx is the final commitment created from evaluating an HTLC
// view at a given height, along with some meta data.
type unsignedCommitmentTx struct {
	// txn is the final, unsigned commitment transaction for this view.
	txn *wire.MsgTx

	// fee is the total fee of the commitment transaction.
	fee btcutil.Amount

	// ourBalance is our balance on this commitment *after* subtracting
	// commitment fees and anchor outputs. This can be different than the
	// balances before creating the commitment transaction as one party must
	// pay the commitment fee.
	ourBalance lnwire.MilliSatoshi

	// theirBalance is their balance of this commitment *after* subtracting
	// commitment fees and anchor outputs. This can be different than the
	// balances before creating the commitment transaction as one party must
	// pay the commitment fee.
	theirBalance lnwire.MilliSatoshi

	// cltvs is a sorted list of CLTV deltas for each HTLC on the commitment
	// transaction. Any non-htlc outputs will have a CLTV delay of zero.
	cltvs []uint32
}

// createUnsignedCommitmentTx generates the unsigned commitment transaction for
// a commitment view and returns it as part of the unsignedCommitmentTx. The
// passed in balances should be balances *before* subtracting any commitment
// fees, but after anchor outputs.
func (cb *CommitmentBuilder) createUnsignedCommitmentTx(ourBalance,
	theirBalance lnwire.MilliSatoshi, isOurs bool,
	feePerKw chainfee.SatPerKWeight, height uint64,
	filteredHTLCView *htlcView,
	keyRing *CommitmentKeyRing) (*unsignedCommitmentTx, error) {

	dustLimit := cb.chanState.LocalChanCfg.DustLimit
	if !isOurs {
		dustLimit = cb.chanState.RemoteChanCfg.DustLimit
	}

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(
			cb.chanState.ChanType, false, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {
			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(
			cb.chanState.ChanType, true, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {
			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := CommitWeight(cb.chanState.ChanType) +
		input.HTLCWeight*numHTLCs

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := feePerKw.FeeForWeight(totalCommitWeight)
	commitFeeMSat := lnwire.NewMSatFromSatoshis(commitFee)

	// Currently, within the protocol, the initiator always pays the fees.
	// So we'll subtract the fee amount from the balance of the current
	// initiator. If the initiator is unable to pay the fee fully, then
	// their entire output is consumed.
	switch {
	case cb.chanState.IsInitiator && commitFee > ourBalance.ToSatoshis():
		ourBalance = 0

	case cb.chanState.IsInitiator:
		ourBalance -= commitFeeMSat

	case !cb.chanState.IsInitiator && commitFee > theirBalance.ToSatoshis():
		theirBalance = 0

	case !cb.chanState.IsInitiator:
		theirBalance -= commitFeeMSat
	}

	var (
		commitTx *wire.MsgTx
		err      error
	)

	// Depending on whether the transaction is ours or not, we call
	// CreateCommitTx with parameters matching the perspective, to generate
	// a new commitment transaction with all the latest unsettled/un-timed
	// out HTLCs.
	if isOurs {
		commitTx, err = CreateCommitTx(
			cb.chanState.ChanType, fundingTxIn(cb.chanState), keyRing,
			&cb.chanState.LocalChanCfg, &cb.chanState.RemoteChanCfg,
			ourBalance.ToSatoshis(), theirBalance.ToSatoshis(),
			numHTLCs,
		)
	} else {
		commitTx, err = CreateCommitTx(
			cb.chanState.ChanType, fundingTxIn(cb.chanState), keyRing,
			&cb.chanState.RemoteChanCfg, &cb.chanState.LocalChanCfg,
			theirBalance.ToSatoshis(), ourBalance.ToSatoshis(),
			numHTLCs,
		)
	}
	if err != nil {
		return nil, err
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
		if htlcIsDust(
			cb.chanState.ChanType, false, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {
			continue
		}

		err := addHTLC(
			commitTx, isOurs, false, htlc, keyRing,
			cb.chanState.ChanType,
		)
		if err != nil {
			return nil, err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(
			cb.chanState.ChanType, true, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {
			continue
		}

		err := addHTLC(
			commitTx, isOurs, true, htlc, keyRing,
			cb.chanState.ChanType,
		)
		if err != nil {
			return nil, err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, height, cb.obfuscator)
	if err != nil {
		return nil, err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	InPlaceCommitSort(commitTx, cltvs)

	// Next, we'll ensure that we don't accidentally create a commitment
	// transaction which would be invalid by consensus.
	uTx := btcutil.NewTx(commitTx)
	if err := blockchain.CheckTransactionSanity(uTx); err != nil {
		return nil, err
	}

	// Finally, we'll assert that were not attempting to draw more out of
	// the channel that was originally placed within it.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	if totalOut > cb.chanState.Capacity {
		return nil, fmt.Errorf("height=%v, for ChannelPoint(%v) "+
			"attempts to consume %v while channel capacity is %v",
			height, cb.chanState.FundingOutpoint,
			totalOut, cb.chanState.Capacity)
	}

	return &unsignedCommitmentTx{
		txn:          commitTx,
		fee:          commitFee,
		ourBalance:   ourBalance,
		theirBalance: theirBalance,
		cltvs:        cltvs,
	}, nil
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one local
// output paying to the "owner" of the commitment transaction which can be
// spent after a relative block delay or revocation event, and a remote output
// paying the counterparty within the channel, which can be spent immediately
// or after a delay depending on the commitment type..
func CreateCommitTx(chanType channeldb.ChannelType,
	fundingOutput wire.TxIn, keyRing *CommitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote btcutil.Amount,
	numHTLCs int64) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	toLocalRedeemScript, err := input.CommitScriptToSelf(
		uint32(localChanCfg.CsvDelay), keyRing.ToLocalKey,
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

	// Next, we create the script paying to the remote.
	toRemoteScript, _, err := CommitScriptToRemote(
		chanType, keyRing.ToRemoteKey,
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
	localOutput := amountToLocal >= localChanCfg.DustLimit
	if localOutput {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toLocalScriptHash,
			Value:    int64(amountToLocal),
		})
	}

	remoteOutput := amountToRemote >= localChanCfg.DustLimit
	if remoteOutput {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toRemoteScript.PkScript,
			Value:    int64(amountToRemote),
		})
	}

	// If this channel type has anchors, we'll also add those.
	if chanType.HasAnchors() {
		localAnchor, remoteAnchor, err := CommitScriptAnchors(
			localChanCfg, remoteChanCfg,
		)
		if err != nil {
			return nil, err
		}

		// Add local anchor output only if we have a commitment output
		// or there are HTLCs.
		if localOutput || numHTLCs > 0 {
			commitTx.AddTxOut(&wire.TxOut{
				PkScript: localAnchor.PkScript,
				Value:    int64(anchorSize),
			})
		}

		// Add anchor output to remote only if they have a commitment
		// output or there are HTLCs.
		if remoteOutput || numHTLCs > 0 {
			commitTx.AddTxOut(&wire.TxOut{
				PkScript: remoteAnchor.PkScript,
				Value:    int64(anchorSize),
			})
		}
	}

	return commitTx, nil
}

// CoopCloseBalance returns the final balances that should be used to create
// the cooperative close tx, given the channel type and transaction fee.
func CoopCloseBalance(chanType channeldb.ChannelType, isInitiator bool,
	coopCloseFee btcutil.Amount, localCommit channeldb.ChannelCommitment) (
	btcutil.Amount, btcutil.Amount, error) {

	// Get both parties' balances from the latest commitment.
	ourBalance := localCommit.LocalBalance.ToSatoshis()
	theirBalance := localCommit.RemoteBalance.ToSatoshis()

	// We'll make sure we account for the complete balance by adding the
	// current dangling commitment fee to the balance of the initiator.
	initiatorDelta := localCommit.CommitFee

	// Since the initiator's balance also is stored after subtracting the
	// anchor values, add that back in case this was an anchor commitment.
	if chanType.HasAnchors() {
		initiatorDelta += 2 * anchorSize
	}

	// The initiator will pay the full coop close fee, subtract that value
	// from their balance.
	initiatorDelta -= coopCloseFee

	if isInitiator {
		ourBalance += initiatorDelta
	} else {
		theirBalance += initiatorDelta
	}

	// During fee negotiation it should always be verified that the
	// initiator can pay the proposed fee, but we do a sanity check just to
	// be sure here.
	if ourBalance < 0 || theirBalance < 0 {
		return 0, 0, fmt.Errorf("initiator cannot afford proposed " +
			"coop close fee")
	}

	return ourBalance, theirBalance, nil
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours.
func genHtlcScript(chanType channeldb.ChannelType, isIncoming, ourCommit bool,
	timeout uint32, rHash [32]byte,
	keyRing *CommitmentKeyRing) ([]byte, []byte, error) {

	var (
		witnessScript []byte
		err           error
	)

	// Choose scripts based on channel type.
	confirmedHtlcSpends := false
	if chanType.HasAnchors() {
		confirmedHtlcSpends = true
	}

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(
			timeout, keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		witnessScript, err = input.SenderHTLCScript(
			keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		witnessScript, err = input.SenderHTLCScript(
			keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(
			timeout, keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)
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
	keyRing *CommitmentKeyRing, chanType channeldb.ChannelType) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	p2wsh, witnessScript, err := genHtlcScript(
		chanType, isIncoming, ourCommit, timeout, rHash, keyRing,
	)
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
