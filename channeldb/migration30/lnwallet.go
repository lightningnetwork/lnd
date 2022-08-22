package migration30

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/input"
)

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

// findOutputIndexesFromRemote finds the index of our and their outputs from
// the remote commitment transaction. It derives the key ring to compute the
// output scripts and compares them against the outputs inside the commitment
// to find the match.
func findOutputIndexesFromRemote(revocationPreimage *chainhash.Hash,
	chanState *mig26.OpenChannel,
	oldLog *mig.ChannelCommitment) (uint32, uint32, error) {

	// Init the output indexes as empty.
	ourIndex := uint32(OutputIndexEmpty)
	theirIndex := uint32(OutputIndexEmpty)

	chanCommit := oldLog
	_, commitmentPoint := btcec.PrivKeyFromBytes(revocationPreimage[:])

	// With the commitment point generated, we can now derive the king ring
	// which will be used to generate the output scripts.
	keyRing := DeriveCommitmentKeys(
		commitmentPoint, false, chanState.ChanType,
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

	// Map the scripts from our PoV. When facing a local commitment, the to
	// local output belongs to us and the to remote output belongs to them.
	// When facing a remote commitment, the to local output belongs to them
	// and the to remote output belongs to us.

	// Compute the to local script. From our PoV, when facing a remote
	// commitment, the to local output belongs to them.
	theirScript, err := CommitScriptToSelf(
		chanState.ChanType, isRemoteInitiator, keyRing.ToLocalKey,
		keyRing.RevocationKey, theirDelay, leaseExpiry,
	)
	if err != nil {
		return ourIndex, theirIndex, err
	}

	// Compute the to remote script. From our PoV, when facing a remote
	// commitment, the to remote output belongs to us.
	ourScript, _, err := CommitScriptToRemote(
		chanState.ChanType, isRemoteInitiator, keyRing.ToRemoteKey,
		leaseExpiry,
	)
	if err != nil {
		return ourIndex, theirIndex, err
	}

	// Now compare the scripts to find our/their output index.
	for i, txOut := range chanCommit.CommitTx.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, ourScript.PkScript):
			ourIndex = uint32(i)
		case bytes.Equal(txOut.PkScript, theirScript.PkScript):
			theirIndex = uint32(i)
		}
	}

	return ourIndex, theirIndex, nil
}

// DeriveCommitmentKeys generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending on the type
// of channel, and whether the commitment transaction is ours or the remote
// peer's.
func DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, chanType mig25.ChannelType,
	localChanCfg, remoteChanCfg *mig.ChannelConfig) *CommitmentKeyRing {

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

// CommitScriptToRemote derives the appropriate to_remote script based on the
// channel's commitment type. The `initiator` argument should correspond to the
// owner of the commitment transaction which we are generating the to_remote
// script for. The second return value is the CSV delay of the output script,
// what must be satisfied in order to spend the output.
func CommitScriptToRemote(chanType mig25.ChannelType, initiator bool,
	key *btcec.PublicKey, leaseExpiry uint32) (*ScriptInfo, uint32, error) {

	switch {
	// If we are not the initiator of a leased channel, then the remote
	// party has an additional CLTV requirement in addition to the 1 block
	// CSV requirement.
	case chanType.HasLeaseExpiration() && !initiator:
		script, err := input.LeaseCommitScriptToRemoteConfirmed(
			key, leaseExpiry,
		)
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

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	case chanType.HasAnchors():
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

	default:
		// Otherwise the to_remote will be a simple p2wkh.
		p2wkh, err := input.CommitScriptUnencumbered(key)
		if err != nil {
			return nil, 0, err
		}

		// Since this is a regular P2WKH, the WitnessScipt and PkScript
		// should both be set to the script hash.
		return &ScriptInfo{
			WitnessScript: p2wkh,
			PkScript:      p2wkh,
		}, 0, nil
	}
}

// CommitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// The `initiator` argument should correspond to the owner of the commitment
// transaction which we are generating the to_local script for. If the other
// party learns of the preimage to the revocation hash, then they can claim all
// the settled funds in the channel, plus the unsettled funds.
func CommitScriptToSelf(chanType mig25.ChannelType, initiator bool,
	selfKey, revokeKey *btcec.PublicKey, csvDelay, leaseExpiry uint32) (
	*ScriptInfo, error) {

	var (
		toLocalRedeemScript []byte
		err                 error
	)
	switch {
	// If we are the initiator of a leased channel, then we have an
	// additional CLTV requirement in addition to the usual CSV requirement.
	case initiator && chanType.HasLeaseExpiration():
		toLocalRedeemScript, err = input.LeaseCommitScriptToSelf(
			selfKey, revokeKey, csvDelay, leaseExpiry,
		)

	default:
		toLocalRedeemScript, err = input.CommitScriptToSelf(
			csvDelay, selfKey, revokeKey,
		)
	}
	if err != nil {
		return nil, err
	}

	toLocalScriptHash, err := input.WitnessScriptHash(toLocalRedeemScript)
	if err != nil {
		return nil, err
	}

	return &ScriptInfo{
		PkScript:      toLocalScriptHash,
		WitnessScript: toLocalRedeemScript,
	}, nil
}
