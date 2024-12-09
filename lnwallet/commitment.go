package lnwallet

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// AnchorSize is the constant anchor output size.
const AnchorSize = btcutil.Amount(330)

// DefaultAnchorsCommitMaxFeeRateSatPerVByte is the default max fee rate in
// sat/vbyte the initiator will use for anchor channels. This should be enough
// to ensure propagation before anchoring down the commitment transaction.
const DefaultAnchorsCommitMaxFeeRateSatPerVByte = 10

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
	whoseCommit lntypes.ChannelParty, chanType channeldb.ChannelType,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *CommitmentKeyRing {

	tweaklessCommit := chanType.IsTweakless()

	// Depending on if this is our commit or not, we'll choose the correct
	// base point.
	localBasePoint := localChanCfg.PaymentBasePoint
	if whoseCommit.IsLocal() {
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
	if whoseCommit.IsLocal() {
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
		if whoseCommit.IsRemote() {
			keyRing.LocalCommitKeyTweak = nil
		}
	} else {
		keyRing.ToRemoteKey = input.TweakPubKey(
			toRemoteBasePoint, commitPoint,
		)
	}

	return keyRing
}

// WitnessScriptDesc holds the output script and the witness script for p2wsh
// outputs.
type WitnessScriptDesc struct {
	// OutputScript is the output's PkScript.
	OutputScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output. This field should be set to the full script if a p2wsh
	// output is being signed. For p2wkh it should be set equal to the
	// PkScript.
	WitnessScript []byte
}

// PkScript is the public key script that commits to the final
// contract.
func (w *WitnessScriptDesc) PkScript() []byte {
	return w.OutputScript
}

// WitnessScriptToSign returns the witness script that we'll use when signing
// for the remote party, and also verifying signatures on our transactions. As
// an example, when we create an outgoing HTLC for the remote party, we want to
// sign their success path.
func (w *WitnessScriptDesc) WitnessScriptToSign() []byte {
	return w.WitnessScript
}

// WitnessScriptForPath returns the witness script for the given spending path.
// An error is returned if the path is unknown. This is useful as when
// constructing a control block for a given path, one also needs witness script
// being signed.
func (w *WitnessScriptDesc) WitnessScriptForPath(
	_ input.ScriptPath) ([]byte, error) {

	return w.WitnessScript, nil
}

// CommitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// The `initiator` argument should correspond to the owner of the commitment
// transaction which we are generating the to_local script for. If the other
// party learns of the preimage to the revocation hash, then they can claim all
// the settled funds in the channel, plus the unsettled funds.
func CommitScriptToSelf(chanType channeldb.ChannelType, initiator bool,
	selfKey, revokeKey *btcec.PublicKey, csvDelay, leaseExpiry uint32,
	auxLeaf input.AuxTapLeaf) (input.ScriptDescriptor, error) {

	switch {
	// For taproot scripts, we'll need to make a slightly modified script
	// where a NUMS key is used to force a script path reveal of either the
	// revocation or the CSV timeout.
	//
	// Our "redeem" script here is just the taproot witness program.
	case chanType.IsTaproot():
		return input.NewLocalCommitScriptTree(
			csvDelay, selfKey, revokeKey, auxLeaf,
		)

	// If we are the initiator of a leased channel, then we have an
	// additional CLTV requirement in addition to the usual CSV
	// requirement.
	case initiator && chanType.HasLeaseExpiration():
		toLocalRedeemScript, err := input.LeaseCommitScriptToSelf(
			selfKey, revokeKey, csvDelay, leaseExpiry,
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

		return &WitnessScriptDesc{
			OutputScript:  toLocalScriptHash,
			WitnessScript: toLocalRedeemScript,
		}, nil

	default:
		toLocalRedeemScript, err := input.CommitScriptToSelf(
			csvDelay, selfKey, revokeKey,
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

		return &WitnessScriptDesc{
			OutputScript:  toLocalScriptHash,
			WitnessScript: toLocalRedeemScript,
		}, nil
	}
}

// CommitScriptToRemote derives the appropriate to_remote script based on the
// channel's commitment type. The `initiator` argument should correspond to the
// owner of the commitment transaction which we are generating the to_remote
// script for. The second return value is the CSV delay of the output script,
// what must be satisfied in order to spend the output.
func CommitScriptToRemote(chanType channeldb.ChannelType, initiator bool,
	remoteKey *btcec.PublicKey, leaseExpiry uint32,
	auxLeaf input.AuxTapLeaf) (input.ScriptDescriptor, uint32, error) {

	switch {
	// If we are not the initiator of a leased channel, then the remote
	// party has an additional CLTV requirement in addition to the 1 block
	// CSV requirement.
	case chanType.HasLeaseExpiration() && !initiator:
		script, err := input.LeaseCommitScriptToRemoteConfirmed(
			remoteKey, leaseExpiry,
		)
		if err != nil {
			return nil, 0, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, 0, err
		}

		return &WitnessScriptDesc{
			OutputScript:  p2wsh,
			WitnessScript: script,
		}, 1, nil

	// For taproot channels, we'll use a slightly different format, where
	// we use a NUMS key to force the remote party to take a script path,
	// with the sole tap leaf enforcing the 1 CSV delay.
	case chanType.IsTaproot():
		toRemoteScriptTree, err := input.NewRemoteCommitScriptTree(
			remoteKey, auxLeaf,
		)
		if err != nil {
			return nil, 0, err
		}

		return toRemoteScriptTree, 1, nil

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	case chanType.HasAnchors():
		script, err := input.CommitScriptToRemoteConfirmed(remoteKey)
		if err != nil {
			return nil, 0, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, 0, err
		}

		return &WitnessScriptDesc{
			OutputScript:  p2wsh,
			WitnessScript: script,
		}, 1, nil

	default:
		// Otherwise the to_remote will be a simple p2wkh.
		p2wkh, err := input.CommitScriptUnencumbered(remoteKey)
		if err != nil {
			return nil, 0, err
		}

		// Since this is a regular P2WKH, the WitnessScipt and PkScript
		// should both be set to the script hash.
		return &WitnessScriptDesc{
			OutputScript:  p2wkh,
			WitnessScript: p2wkh,
		}, 0, nil
	}
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

// sweepSigHash returns the sign descriptor to use when signing a sweep
// transaction. For taproot channels, we'll use this to always sweep with
// sighash default.
func sweepSigHash(chanType channeldb.ChannelType) txscript.SigHashType {
	if chanType.IsTaproot() {
		return txscript.SigHashDefault
	}

	return txscript.SigHashAll
}

// SecondLevelHtlcScript derives the appropriate second level HTLC script based
// on the channel's commitment type. It is the uniform script that's used as the
// output for the second-level HTLC transactions. The second level transaction
// act as a sort of covenant, ensuring that a 2-of-2 multi-sig output can only
// be spent in a particular way, and to a particular output. The `initiator`
// argument should correspond to the owner of the commitment transaction which
// we are generating the to_local script for.
func SecondLevelHtlcScript(chanType channeldb.ChannelType, initiator bool,
	revocationKey, delayKey *btcec.PublicKey, csvDelay, leaseExpiry uint32,
	auxLeaf input.AuxTapLeaf) (input.ScriptDescriptor, error) {

	switch {
	// For taproot channels, the pkScript is a segwit v1 p2tr output.
	case chanType.IsTaproot():
		return input.TaprootSecondLevelScriptTree(
			revocationKey, delayKey, csvDelay, auxLeaf,
		)

	// If we are the initiator of a leased channel, then we have an
	// additional CLTV requirement in addition to the usual CSV
	// requirement.
	case initiator && chanType.HasLeaseExpiration():
		witnessScript, err := input.LeaseSecondLevelHtlcScript(
			revocationKey, delayKey, csvDelay, leaseExpiry,
		)
		if err != nil {
			return nil, err
		}

		pkScript, err := input.WitnessScriptHash(witnessScript)
		if err != nil {
			return nil, err
		}

		return &WitnessScriptDesc{
			OutputScript:  pkScript,
			WitnessScript: witnessScript,
		}, nil

	default:
		witnessScript, err := input.SecondLevelHtlcScript(
			revocationKey, delayKey, csvDelay,
		)
		if err != nil {
			return nil, err
		}

		pkScript, err := input.WitnessScriptHash(witnessScript)
		if err != nil {
			return nil, err
		}

		return &WitnessScriptDesc{
			OutputScript:  pkScript,
			WitnessScript: witnessScript,
		}, nil
	}
}

// CommitWeight returns the base commitment weight before adding HTLCs.
func CommitWeight(chanType channeldb.ChannelType) lntypes.WeightUnit {
	switch {
	case chanType.IsTaproot():
		return input.TaprootCommitWeight

	// If this commitment has anchors, it will be slightly heavier.
	case chanType.HasAnchors():
		return input.AnchorCommitWeight

	default:
		return input.CommitWeight
	}
}

// HtlcTimeoutFee returns the fee in satoshis required for an HTLC timeout
// transaction based on the current fee rate.
func HtlcTimeoutFee(chanType channeldb.ChannelType,
	feePerKw chainfee.SatPerKWeight) btcutil.Amount {

	switch {
	// For zero-fee HTLC channels, this will always be zero, regardless of
	// feerate.
	case chanType.ZeroHtlcTxFee() || chanType.IsTaproot():
		return 0

	case chanType.HasAnchors():
		return feePerKw.FeeForWeight(input.HtlcTimeoutWeightConfirmed)

	default:
		return feePerKw.FeeForWeight(input.HtlcTimeoutWeight)
	}
}

// HtlcSuccessFee returns the fee in satoshis required for an HTLC success
// transaction based on the current fee rate.
func HtlcSuccessFee(chanType channeldb.ChannelType,
	feePerKw chainfee.SatPerKWeight) btcutil.Amount {

	switch {
	// For zero-fee HTLC channels, this will always be zero, regardless of
	// feerate.
	case chanType.ZeroHtlcTxFee() || chanType.IsTaproot():
		return 0

	case chanType.HasAnchors():
		return feePerKw.FeeForWeight(input.HtlcSuccessWeightConfirmed)

	default:
		return feePerKw.FeeForWeight(input.HtlcSuccessWeight)
	}
}

// CommitScriptAnchors return the scripts to use for the local and remote
// anchor.
func CommitScriptAnchors(chanType channeldb.ChannelType,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	keyRing *CommitmentKeyRing) (
	input.ScriptDescriptor, input.ScriptDescriptor, error) {

	var (
		anchorScript func(
			key *btcec.PublicKey) (input.ScriptDescriptor, error)

		keySelector func(*channeldb.ChannelConfig,
			bool) *btcec.PublicKey
	)

	switch {
	// For taproot channels, the anchor is slightly different: the top
	// level key is now the (relative) local delay and remote public key,
	// since these are fully revealed once the commitment hits the chain.
	case chanType.IsTaproot():
		anchorScript = func(
			key *btcec.PublicKey) (input.ScriptDescriptor, error) {

			return input.NewAnchorScriptTree(key)
		}

		keySelector = func(cfg *channeldb.ChannelConfig,
			local bool) *btcec.PublicKey {

			if local {
				return keyRing.ToLocalKey
			}

			return keyRing.ToRemoteKey
		}

	// For normal channels we'll use the multi-sig keys since those are
	// revealed when the channel closes
	default:
		// For normal channels, we'll create a p2wsh script based on
		// the target key.
		anchorScript = func(
			key *btcec.PublicKey) (input.ScriptDescriptor, error) {

			script, err := input.CommitScriptAnchor(key)
			if err != nil {
				return nil, err
			}

			scriptHash, err := input.WitnessScriptHash(script)
			if err != nil {
				return nil, err
			}

			return &WitnessScriptDesc{
				OutputScript:  scriptHash,
				WitnessScript: script,
			}, nil
		}

		// For the existing channels, we'll always select the multi-sig
		// key from the party's channel config.
		keySelector = func(cfg *channeldb.ChannelConfig,
			_ bool) *btcec.PublicKey {

			return cfg.MultiSigKey.PubKey
		}
	}

	// Get the script used for the anchor output spendable by the local
	// node.
	localAnchor, err := anchorScript(keySelector(localChanCfg, true))
	if err != nil {
		return nil, nil, err
	}

	// And the anchor spendable by the remote node.
	remoteAnchor, err := anchorScript(keySelector(remoteChanCfg, false))
	if err != nil {
		return nil, nil, err
	}

	return localAnchor, remoteAnchor, nil
}

// CommitmentBuilder is a type that wraps the type of channel we are dealing
// with, and abstracts the various ways of constructing commitment
// transactions.
type CommitmentBuilder struct {
	// chanState is the underlying channel's state struct, used to
	// determine the type of channel we are dealing with, and relevant
	// parameters.
	chanState *channeldb.OpenChannel

	// obfuscator is a 48-bit state hint that's used to obfuscate the
	// current state number on the commitment transactions.
	obfuscator [StateHintSize]byte

	// auxLeafStore is an interface that allows us to fetch auxiliary
	// tapscript leaves for the commitment output.
	auxLeafStore fn.Option[AuxLeafStore]
}

// NewCommitmentBuilder creates a new CommitmentBuilder from chanState.
func NewCommitmentBuilder(chanState *channeldb.OpenChannel,
	leafStore fn.Option[AuxLeafStore]) *CommitmentBuilder {

	// The anchor channel type MUST be tweakless.
	if chanState.ChanType.HasAnchors() && !chanState.ChanType.IsTweakless() {
		panic("invalid channel type combination")
	}

	return &CommitmentBuilder{
		chanState:    chanState,
		obfuscator:   createStateHintObfuscator(chanState),
		auxLeafStore: leafStore,
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
	theirBalance lnwire.MilliSatoshi, whoseCommit lntypes.ChannelParty,
	feePerKw chainfee.SatPerKWeight, height uint64, originalHtlcView,
	filteredHTLCView *HtlcView, keyRing *CommitmentKeyRing,
	prevCommit *commitment) (*unsignedCommitmentTx, error) {

	dustLimit := cb.chanState.LocalChanCfg.DustLimit
	if whoseCommit.IsRemote() {
		dustLimit = cb.chanState.RemoteChanCfg.DustLimit
	}

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.Updates.Local {
		if HtlcIsDust(
			cb.chanState.ChanType, false, whoseCommit, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.Updates.Remote {
		if HtlcIsDust(
			cb.chanState.ChanType, true, whoseCommit, feePerKw,
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
		lntypes.WeightUnit(input.HTLCWeight*numHTLCs)

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

	var commitTx *wire.MsgTx

	// Before we create the commitment transaction below, we'll try to see
	// if there're any aux leaves that need to be a part of the tapscript
	// tree. We'll only do this if we have a custom blob defined though.
	auxResult, err := fn.MapOptionZ(
		cb.auxLeafStore,
		func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return auxLeavesFromView(
				s, cb.chanState, prevCommit.customBlob,
				originalHtlcView, whoseCommit, ourBalance,
				theirBalance, *keyRing,
			)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// Depending on whether the transaction is ours or not, we call
	// CreateCommitTx with parameters matching the perspective, to generate
	// a new commitment transaction with all the latest unsettled/un-timed
	// out HTLCs.
	var leaseExpiry uint32
	if cb.chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = cb.chanState.ThawHeight
	}
	if whoseCommit.IsLocal() {
		commitTx, err = CreateCommitTx(
			cb.chanState.ChanType, fundingTxIn(cb.chanState), keyRing,
			&cb.chanState.LocalChanCfg, &cb.chanState.RemoteChanCfg,
			ourBalance.ToSatoshis(), theirBalance.ToSatoshis(),
			numHTLCs, cb.chanState.IsInitiator, leaseExpiry,
			auxResult.AuxLeaves,
		)
	} else {
		commitTx, err = CreateCommitTx(
			cb.chanState.ChanType, fundingTxIn(cb.chanState), keyRing,
			&cb.chanState.RemoteChanCfg, &cb.chanState.LocalChanCfg,
			theirBalance.ToSatoshis(), ourBalance.ToSatoshis(),
			numHTLCs, !cb.chanState.IsInitiator, leaseExpiry,
			auxResult.AuxLeaves,
		)
	}
	if err != nil {
		return nil, err
	}

	// Similarly, we'll now attempt to extract the set of aux leaves for
	// the set of incoming and outgoing HTLCs.
	incomingAuxLeaves := fn.MapOption(
		func(leaves CommitAuxLeaves) input.HtlcAuxLeaves {
			return leaves.IncomingHtlcLeaves
		},
	)(auxResult.AuxLeaves)
	outgoingAuxLeaves := fn.MapOption(
		func(leaves CommitAuxLeaves) input.HtlcAuxLeaves {
			return leaves.OutgoingHtlcLeaves
		},
	)(auxResult.AuxLeaves)

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
	htlcIndexes := make([]input.HtlcIndex, len(commitTx.TxOut))
	for _, htlc := range filteredHTLCView.Updates.Local {
		if HtlcIsDust(
			cb.chanState.ChanType, false, whoseCommit, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		auxLeaf := fn.FlatMapOption(
			func(leaves input.HtlcAuxLeaves) input.AuxTapLeaf {
				return leaves[htlc.HtlcIndex].AuxTapLeaf
			},
		)(outgoingAuxLeaves)

		err := addHTLC(
			commitTx, whoseCommit, false, htlc, keyRing,
			cb.chanState.ChanType, auxLeaf,
		)
		if err != nil {
			return nil, err
		}

		// We want to add the CLTV and HTLC index to their respective
		// slices, even if we already pre-allocated them.
		cltvs = append(cltvs, htlc.Timeout)               //nolint
		htlcIndexes = append(htlcIndexes, htlc.HtlcIndex) //nolint
	}
	for _, htlc := range filteredHTLCView.Updates.Remote {
		if HtlcIsDust(
			cb.chanState.ChanType, true, whoseCommit, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		auxLeaf := fn.FlatMapOption(
			func(leaves input.HtlcAuxLeaves) input.AuxTapLeaf {
				return leaves[htlc.HtlcIndex].AuxTapLeaf
			},
		)(incomingAuxLeaves)

		err := addHTLC(
			commitTx, whoseCommit, true, htlc, keyRing,
			cb.chanState.ChanType, auxLeaf,
		)
		if err != nil {
			return nil, err
		}

		// We want to add the CLTV and HTLC index to their respective
		// slices, even if we already pre-allocated them.
		cltvs = append(cltvs, htlc.Timeout)               //nolint
		htlcIndexes = append(htlcIndexes, htlc.HtlcIndex) //nolint
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, height, cb.obfuscator)
	if err != nil {
		return nil, err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering (which might be customized for custom channel types, but
	// deterministic and both parties will arrive at the same result). This
	// lets us skip sending the entire transaction over, instead we'll just
	// send signatures.
	commitSort := auxResult.CommitSortFunc.UnwrapOr(DefaultCommitSort)
	err = commitSort(commitTx, cltvs, htlcIndexes)
	if err != nil {
		return nil, fmt.Errorf("unable to sort commitment "+
			"transaction: %w", err)
	}

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
	if totalOut+commitFee > cb.chanState.Capacity {
		return nil, fmt.Errorf("height=%v, for ChannelPoint(%v) "+
			"attempts to consume %v while channel capacity is %v",
			height, cb.chanState.FundingOutpoint,
			totalOut+commitFee, cb.chanState.Capacity)
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
// or after a delay depending on the commitment type. The `initiator` argument
// should correspond to the owner of the commitment transaction we are creating.
func CreateCommitTx(chanType channeldb.ChannelType,
	fundingOutput wire.TxIn, keyRing *CommitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote btcutil.Amount,
	numHTLCs int64, initiator bool, leaseExpiry uint32,
	auxLeaves fn.Option[CommitAuxLeaves]) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	localAuxLeaf := fn.MapOption(func(l CommitAuxLeaves) input.AuxTapLeaf {
		return l.LocalAuxLeaf
	})(auxLeaves)
	toLocalScript, err := CommitScriptToSelf(
		chanType, initiator, keyRing.ToLocalKey, keyRing.RevocationKey,
		uint32(localChanCfg.CsvDelay), leaseExpiry,
		fn.FlattenOption(localAuxLeaf),
	)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to the remote.
	remoteAuxLeaf := fn.MapOption(func(l CommitAuxLeaves) input.AuxTapLeaf {
		return l.RemoteAuxLeaf
	})(auxLeaves)
	toRemoteScript, _, err := CommitScriptToRemote(
		chanType, initiator, keyRing.ToRemoteKey, leaseExpiry,
		fn.FlattenOption(remoteAuxLeaf),
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
			PkScript: toLocalScript.PkScript(),
			Value:    int64(amountToLocal),
		})
	}

	remoteOutput := amountToRemote >= localChanCfg.DustLimit
	if remoteOutput {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toRemoteScript.PkScript(),
			Value:    int64(amountToRemote),
		})
	}

	// If this channel type has anchors, we'll also add those.
	if chanType.HasAnchors() {
		localAnchor, remoteAnchor, err := CommitScriptAnchors(
			chanType, localChanCfg, remoteChanCfg, keyRing,
		)
		if err != nil {
			return nil, err
		}

		// Add local anchor output only if we have a commitment output
		// or there are HTLCs.
		if localOutput || numHTLCs > 0 {
			commitTx.AddTxOut(&wire.TxOut{
				PkScript: localAnchor.PkScript(),
				Value:    int64(AnchorSize),
			})
		}

		// Add anchor output to remote only if they have a commitment
		// output or there are HTLCs.
		if remoteOutput || numHTLCs > 0 {
			commitTx.AddTxOut(&wire.TxOut{
				PkScript: remoteAnchor.PkScript(),
				Value:    int64(AnchorSize),
			})
		}
	}

	return commitTx, nil
}

// CoopCloseBalance returns the final balances that should be used to create
// the cooperative close tx, given the channel type and transaction fee.
func CoopCloseBalance(chanType channeldb.ChannelType, isInitiator bool,
	coopCloseFee, ourBalance, theirBalance, commitFee btcutil.Amount,
	feePayer fn.Option[lntypes.ChannelParty],
) (btcutil.Amount, btcutil.Amount, error) {

	// We'll make sure we account for the complete balance by adding the
	// current dangling commitment fee to the balance of the initiator.
	initiatorDelta := commitFee

	// Since the initiator's balance also is stored after subtracting the
	// anchor values, add that back in case this was an anchor commitment.
	if chanType.HasAnchors() {
		initiatorDelta += 2 * AnchorSize
	}

	// To start with, we'll add the anchor and/or commitment fee to the
	// balance of the initiator.
	if isInitiator {
		ourBalance += initiatorDelta
	} else {
		theirBalance += initiatorDelta
	}

	// With the initiator's balance credited, we'll now subtract the closing
	// fee from the closing party. By default, the initiator pays the full
	// amount, but this can be overridden by the feePayer option.
	defaultPayer := func() lntypes.ChannelParty {
		if isInitiator {
			return lntypes.Local
		}

		return lntypes.Remote
	}()
	payer := feePayer.UnwrapOr(defaultPayer)

	// Based on the payer computed above, we'll subtract the closing fee.
	switch payer {
	case lntypes.Local:
		ourBalance -= coopCloseFee
	case lntypes.Remote:
		theirBalance -= coopCloseFee
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

// genSegwitV0HtlcScript generates the HTLC scripts for a normal segwit v0
// channel.
func genSegwitV0HtlcScript(chanType channeldb.ChannelType,
	isIncoming bool, whoseCommit lntypes.ChannelParty, timeout uint32,
	rHash [32]byte, keyRing *CommitmentKeyRing,
) (*WitnessScriptDesc, error) {

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
	// transaction. So we need to use the receiver's version of the HTLC
	// script.
	case isIncoming && whoseCommit.IsLocal():
		witnessScript, err = input.ReceiverHTLCScript(
			timeout, keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && whoseCommit.IsRemote():
		witnessScript, err = input.SenderHTLCScript(
			keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && whoseCommit.IsLocal():
		witnessScript, err = input.SenderHTLCScript(
			keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && whoseCommit.IsRemote():
		witnessScript, err = input.ReceiverHTLCScript(
			timeout, keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], confirmedHtlcSpends,
		)
	}
	if err != nil {
		return nil, err
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, err
	}

	return &WitnessScriptDesc{
		OutputScript:  htlcP2WSH,
		WitnessScript: witnessScript,
	}, nil
}

// GenTaprootHtlcScript generates the HTLC scripts for a taproot+musig2
// channel.
func GenTaprootHtlcScript(isIncoming bool, whoseCommit lntypes.ChannelParty,
	timeout uint32, rHash [32]byte, keyRing *CommitmentKeyRing,
	auxLeaf input.AuxTapLeaf) (*input.HtlcScriptTree, error) {

	var (
		htlcScriptTree *input.HtlcScriptTree
		err            error
	)

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && whoseCommit.IsLocal():
		htlcScriptTree, err = input.ReceiverHTLCScriptTaproot(
			timeout, keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], whoseCommit, auxLeaf,
		)

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && whoseCommit.IsRemote():
		htlcScriptTree, err = input.SenderHTLCScriptTaproot(
			keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:], whoseCommit, auxLeaf,
		)

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && whoseCommit.IsLocal():
		htlcScriptTree, err = input.SenderHTLCScriptTaproot(
			keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], whoseCommit, auxLeaf,
		)

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && whoseCommit.IsRemote():
		htlcScriptTree, err = input.ReceiverHTLCScriptTaproot(
			timeout, keyRing.LocalHtlcKey, keyRing.RemoteHtlcKey,
			keyRing.RevocationKey, rHash[:], whoseCommit, auxLeaf,
		)
	}

	return htlcScriptTree, err
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours. A script
// multiplexer for the various spending paths is returned. The script path that
// we need to sign for the remote party (2nd level HTLCs) is also returned
// along side the multiplexer.
func genHtlcScript(chanType channeldb.ChannelType, isIncoming bool,
	whoseCommit lntypes.ChannelParty, timeout uint32, rHash [32]byte,
	keyRing *CommitmentKeyRing,
	auxLeaf input.AuxTapLeaf) (input.ScriptDescriptor, error) {

	if !chanType.IsTaproot() {
		return genSegwitV0HtlcScript(
			chanType, isIncoming, whoseCommit, timeout, rHash,
			keyRing,
		)
	}

	return GenTaprootHtlcScript(
		isIncoming, whoseCommit, timeout, rHash, keyRing, auxLeaf,
	)
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// paymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func addHTLC(commitTx *wire.MsgTx, whoseCommit lntypes.ChannelParty,
	isIncoming bool, paymentDesc *paymentDescriptor,
	keyRing *CommitmentKeyRing, chanType channeldb.ChannelType,
	auxLeaf input.AuxTapLeaf) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	scriptInfo, err := genHtlcScript(
		chanType, isIncoming, whoseCommit, timeout, rHash, keyRing,
		auxLeaf,
	)
	if err != nil {
		return err
	}

	pkScript := scriptInfo.PkScript()

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount.ToSatoshis())
	commitTx.AddTxOut(wire.NewTxOut(amountPending, pkScript))

	// Store the pkScript of this particular paymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if whoseCommit.IsLocal() {
		paymentDesc.ourPkScript = pkScript

		paymentDesc.ourWitnessScript = scriptInfo.WitnessScriptToSign()
	} else {
		paymentDesc.theirPkScript = pkScript

		//nolint:ll
		paymentDesc.theirWitnessScript = scriptInfo.WitnessScriptToSign()
	}

	return nil
}

// findOutputIndexesFromRemote finds the index of our and their outputs from
// the remote commitment transaction. It derives the key ring to compute the
// output scripts and compares them against the outputs inside the commitment
// to find the match.
func findOutputIndexesFromRemote(revocationPreimage *chainhash.Hash,
	chanState *channeldb.OpenChannel,
	leafStore fn.Option[AuxLeafStore]) (uint32, uint32, error) {

	// Init the output indexes as empty.
	ourIndex := uint32(channeldb.OutputIndexEmpty)
	theirIndex := uint32(channeldb.OutputIndexEmpty)

	chanCommit := chanState.RemoteCommitment
	_, commitmentPoint := btcec.PrivKeyFromBytes(revocationPreimage[:])

	// With the commitment point generated, we can now derive the king ring
	// which will be used to generate the output scripts.
	keyRing := DeriveCommitmentKeys(
		commitmentPoint, lntypes.Remote, chanState.ChanType,
		&chanState.LocalChanCfg, &chanState.RemoteChanCfg,
	)

	// Since it's remote commitment chain, we'd used the mirrored values.
	//
	// We use the remote's channel config for the csv delay.
	theirDelay := uint32(chanState.RemoteChanCfg.CsvDelay)

	// If we are the initiator of this channel, then it's be false from the
	// remote's PoV.
	isRemoteInitiator := !chanState.IsInitiator

	var leaseExpiry uint32
	if chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = chanState.ThawHeight
	}

	// If we have a custom blob, then we'll attempt to fetch the aux leaves
	// for this state.
	auxResult, err := fn.MapOptionZ(
		leafStore, func(a AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return a.FetchLeavesFromCommit(
				NewAuxChanState(chanState), chanCommit,
				*keyRing, lntypes.Remote,
			)
		},
	).Unpack()
	if err != nil {
		return ourIndex, theirIndex, fmt.Errorf("unable to fetch aux "+
			"leaves: %w", err)
	}

	// Map the scripts from our PoV. When facing a local commitment, the
	// to_local output belongs to us and the to_remote output belongs to
	// them. When facing a remote commitment, the to_local output belongs to
	// them and the to_remote output belongs to us.

	// Compute the to_local script. From our PoV, when facing a remote
	// commitment, the to_local output belongs to them.
	localAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.LocalAuxLeaf
		},
	)(auxResult.AuxLeaves)
	theirScript, err := CommitScriptToSelf(
		chanState.ChanType, isRemoteInitiator, keyRing.ToLocalKey,
		keyRing.RevocationKey, theirDelay, leaseExpiry, localAuxLeaf,
	)
	if err != nil {
		return ourIndex, theirIndex, err
	}

	// Compute the to_remote script. From our PoV, when facing a remote
	// commitment, the to_remote output belongs to us.
	remoteAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.RemoteAuxLeaf
		},
	)(auxResult.AuxLeaves)
	ourScript, _, err := CommitScriptToRemote(
		chanState.ChanType, isRemoteInitiator, keyRing.ToRemoteKey,
		leaseExpiry, remoteAuxLeaf,
	)
	if err != nil {
		return ourIndex, theirIndex, err
	}

	// Now compare the scripts to find our/their output index.
	for i, txOut := range chanCommit.CommitTx.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, ourScript.PkScript()):
			ourIndex = uint32(i)
		case bytes.Equal(txOut.PkScript, theirScript.PkScript()):
			theirIndex = uint32(i)
		}
	}

	return ourIndex, theirIndex, nil
}
