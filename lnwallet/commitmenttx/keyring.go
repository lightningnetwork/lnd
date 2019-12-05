package commitmenttx

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
)

// KeyRing holds all derived keys needed to construct commitment and
// HTLC transactions. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's. Private keys associated
// with each key may belong to the commitment owner or the "other party" which
// is referred to in the field comments, regardless of which is local and which
// is remote.
type KeyRing struct {
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

// deriveCommitmentKeys generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's.
func deriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit, tweaklessCommit bool,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *KeyRing {

	// First, we'll derive all the keys that don't depend on the context of
	// whose commitment transaction this is.
	keyRing := &KeyRing{
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
