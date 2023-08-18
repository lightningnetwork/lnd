package tweaks

import (
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
)

// SingleTweakBytes computes set of bytes we call the single tweak. The purpose
// of the single tweak is to randomize all regular delay and payment base
// points. To do this, we generate a hash that binds the commitment point to
// the pay/delay base point. The end end results is that the basePoint is
// tweaked as follows:
//
//   - key = basePoint + sha256(commitPoint || basePoint)*G
func SingleTweakBytes(commitPoint, basePoint *btcec.PublicKey) []byte {
	h := sha256.New()
	h.Write(commitPoint.SerializeCompressed())
	h.Write(basePoint.SerializeCompressed())
	return h.Sum(nil)
}

// TweakPubKey tweaks a public base point given a per commitment point. The per
// commitment point is a unique point on our target curve for each commitment
// transaction. When tweaking a local base point for use in a remote commitment
// transaction, the remote party's current per commitment point is to be used.
// The opposite applies for when tweaking remote keys. Precisely, the following
// operation is used to "tweak" public keys:
//
//	tweakPub := basePoint + sha256(commitPoint || basePoint) * G
//	         := G*k + sha256(commitPoint || basePoint)*G
//	         := G*(k + sha256(commitPoint || basePoint))
//
// Therefore, if a party possess the value k, the private key of the base
// point, then they are able to derive the proper private key for the
// revokeKey by computing:
//
//	revokePriv := k + sha256(commitPoint || basePoint) mod N
//
// Where N is the order of the sub-group.
//
// The rationale for tweaking all public keys used within the commitment
// contracts is to ensure that all keys are properly delinearized to avoid any
// funny business when jointly collaborating to compute public and private
// keys. Additionally, the use of the per commitment point ensures that each
// commitment state houses a unique set of keys which is useful when creating
// blinded channel outsourcing protocols.
//
// TODO(roasbeef): should be using double-scalar mult here
func TweakPubKey(basePoint, commitPoint *btcec.PublicKey) *btcec.PublicKey {
	tweakBytes := SingleTweakBytes(commitPoint, basePoint)
	return TweakPubKeyWithTweak(basePoint, tweakBytes)
}

// TweakPubKeyWithTweak is the exact same as the TweakPubKey function, however
// it accepts the raw tweak bytes directly rather than the commitment point.
func TweakPubKeyWithTweak(pubKey *btcec.PublicKey,
	tweakBytes []byte) *btcec.PublicKey {

	var (
		pubKeyJacobian btcec.JacobianPoint
		tweakJacobian  btcec.JacobianPoint
		resultJacobian btcec.JacobianPoint
	)
	tweakKey, _ := btcec.PrivKeyFromBytes(tweakBytes)
	btcec.ScalarBaseMultNonConst(&tweakKey.Key, &tweakJacobian)

	pubKey.AsJacobian(&pubKeyJacobian)
	btcec.AddNonConst(&pubKeyJacobian, &tweakJacobian, &resultJacobian)

	resultJacobian.ToAffine()
	return btcec.NewPublicKey(&resultJacobian.X, &resultJacobian.Y)
}

// TweakPrivKey tweaks the private key of a public base point given a per
// commitment point. The per commitment secret is the revealed revocation
// secret for the commitment state in question. This private key will only need
// to be generated in the case that a channel counter party broadcasts a
// revoked state. Precisely, the following operation is used to derive a
// tweaked private key:
//
//   - tweakPriv := basePriv + sha256(commitment || basePub) mod N
//
// Where N is the order of the sub-group.
func TweakPrivKey(basePriv *btcec.PrivateKey,
	commitTweak []byte) *btcec.PrivateKey {

	// tweakInt := sha256(commitPoint || basePub)
	tweakScalar := new(btcec.ModNScalar)
	tweakScalar.SetByteSlice(commitTweak)

	tweakScalar.Add(&basePriv.Key)

	return &btcec.PrivateKey{Key: *tweakScalar}
}

// DeriveRevocationPubkey derives the revocation public key given the
// counterparty's commitment key, and revocation preimage derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// preimage, then they'll be able to derive the corresponding private key to
// this private key by exploiting the homomorphism in the elliptic curve group:
//   - https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//	revokeKey := revokeBase * sha256(revocationBase || commitPoint) +
//	             commitPoint * sha256(commitPoint || revocationBase)
//
//	          := G*(revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	             G*(commitSecret * sha256(commitPoint || revocationBase))
//
//	          := G*(revokeBasePriv * sha256(revocationBase || commitPoint) +
//	                commitSecret * sha256(commitPoint || revocationBase))
//
// Therefore, once we divulge the revocation secret, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//
//	revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	              (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(revokeBase,
	commitPoint *btcec.PublicKey) *btcec.PublicKey {

	// R = revokeBase * sha256(revocationBase || commitPoint)
	revokeTweakBytes := SingleTweakBytes(revokeBase, commitPoint)
	revokeTweakScalar := new(btcec.ModNScalar)
	revokeTweakScalar.SetByteSlice(revokeTweakBytes)

	var (
		revokeBaseJacobian btcec.JacobianPoint
		rJacobian          btcec.JacobianPoint
	)
	revokeBase.AsJacobian(&revokeBaseJacobian)
	btcec.ScalarMultNonConst(
		revokeTweakScalar, &revokeBaseJacobian, &rJacobian,
	)

	// C = commitPoint * sha256(commitPoint || revocationBase)
	commitTweakBytes := SingleTweakBytes(commitPoint, revokeBase)
	commitTweakScalar := new(btcec.ModNScalar)
	commitTweakScalar.SetByteSlice(commitTweakBytes)

	var (
		commitPointJacobian btcec.JacobianPoint
		cJacobian           btcec.JacobianPoint
	)
	commitPoint.AsJacobian(&commitPointJacobian)
	btcec.ScalarMultNonConst(
		commitTweakScalar, &commitPointJacobian, &cJacobian,
	)

	// Now that we have the revocation point, we add this to their commitment
	// public key in order to obtain the revocation public key.
	//
	// P = R + C
	var resultJacobian btcec.JacobianPoint
	btcec.AddNonConst(&rJacobian, &cJacobian, &resultJacobian)

	resultJacobian.ToAffine()
	return btcec.NewPublicKey(&resultJacobian.X, &resultJacobian.Y)
}

// DeriveRevocationPrivKey derives the revocation private key given a node's
// commitment private key, and the preimage to a previously seen revocation
// hash. Using this derived private key, a node is able to claim the output
// within the commitment transaction of a node in the case that they broadcast
// a previously revoked commitment transaction.
//
// The private key is derived as follows:
//
//	revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	              (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPrivKey(revokeBasePriv *btcec.PrivateKey,
	commitSecret *btcec.PrivateKey) *btcec.PrivateKey {

	// r = sha256(revokeBasePub || commitPoint)
	revokeTweakBytes := SingleTweakBytes(
		revokeBasePriv.PubKey(), commitSecret.PubKey(),
	)
	revokeTweakScalar := new(btcec.ModNScalar)
	revokeTweakScalar.SetByteSlice(revokeTweakBytes)

	// c = sha256(commitPoint || revokeBasePub)
	commitTweakBytes := SingleTweakBytes(
		commitSecret.PubKey(), revokeBasePriv.PubKey(),
	)
	commitTweakScalar := new(btcec.ModNScalar)
	commitTweakScalar.SetByteSlice(commitTweakBytes)

	// Finally to derive the revocation secret key we'll perform the
	// following operation:
	//
	//  k = (revocationPriv * r) + (commitSecret * c) mod N
	//
	// This works since:
	//  P = (G*a)*b + (G*c)*d
	//  P = G*(a*b) + G*(c*d)
	//  P = G*(a*b + c*d)
	revokeHalfPriv := revokeTweakScalar.Mul(&revokeBasePriv.Key)
	commitHalfPriv := commitTweakScalar.Mul(&commitSecret.Key)

	revocationPriv := revokeHalfPriv.Add(commitHalfPriv)

	return &btcec.PrivateKey{Key: *revocationPriv}
}

// ComputeCommitmentPoint generates a commitment point given a commitment
// secret. The commitment point for each state is used to randomize each key in
// the key-ring and also to used as a tweak to derive new public+private keys
// for the state.
func ComputeCommitmentPoint(commitSecret []byte) *btcec.PublicKey {
	_, pubKey := btcec.PrivKeyFromBytes(commitSecret)
	return pubKey
}
