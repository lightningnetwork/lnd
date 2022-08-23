package keychain

import (
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
)

// NewPubKeyECDH wraps the given key of the key ring so it adheres to the
// SingleKeyECDH interface.
func NewPubKeyECDH(keyDesc KeyDescriptor, ecdh ECDHRing) *PubKeyECDH {
	return &PubKeyECDH{
		keyDesc: keyDesc,
		ecdh:    ecdh,
	}
}

// PubKeyECDH is an implementation of the SingleKeyECDH interface. It wraps an
// ECDH key ring so it can perform ECDH shared key generation against a single
// abstracted away private key.
type PubKeyECDH struct {
	keyDesc KeyDescriptor
	ecdh    ECDHRing
}

// PubKey returns the public key of the private key that is abstracted away by
// the interface.
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PubKeyECDH) PubKey() *btcec.PublicKey {
	return p.keyDesc.PubKey
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// abstracted private key and a remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PubKeyECDH) ECDH(pubKey *btcec.PublicKey) ([32]byte, error) {
	return p.ecdh.ECDH(p.keyDesc, pubKey)
}

// PrivKeyECDH is an implementation of the SingleKeyECDH in which we do have the
// full private key. This can be used to wrap a temporary key to conform to the
// SingleKeyECDH interface.
type PrivKeyECDH struct {
	// PrivKey is the private key that is used for the ECDH operation.
	PrivKey *btcec.PrivateKey
}

// PubKey returns the public key of the private key that is abstracted away by
// the interface.
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PrivKeyECDH) PubKey() *btcec.PublicKey {
	return p.PrivKey.PubKey()
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// abstracted private key and a remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PrivKeyECDH) ECDH(pub *btcec.PublicKey) ([32]byte, error) {
	var (
		pubJacobian btcec.JacobianPoint
		s           btcec.JacobianPoint
	)
	pub.AsJacobian(&pubJacobian)

	btcec.ScalarMultNonConst(&p.PrivKey.Key, &pubJacobian, &s)
	s.ToAffine()
	sPubKey := btcec.NewPublicKey(&s.X, &s.Y)
	return sha256.Sum256(sPubKey.SerializeCompressed()), nil
}

var _ SingleKeyECDH = (*PubKeyECDH)(nil)
var _ SingleKeyECDH = (*PrivKeyECDH)(nil)
