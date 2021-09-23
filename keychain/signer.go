package keychain

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func NewPubKeyDigestSigner(keyDesc KeyDescriptor,
	signer DigestSignerRing) *PubKeyDigestSigner {

	return &PubKeyDigestSigner{
		keyDesc:      keyDesc,
		digestSigner: signer,
	}
}

type PubKeyDigestSigner struct {
	keyDesc      KeyDescriptor
	digestSigner DigestSignerRing
}

func (p *PubKeyDigestSigner) PubKey() *btcec.PublicKey {
	return p.keyDesc.PubKey
}

func (p *PubKeyDigestSigner) SignMessage(message []byte,
	doubleHash bool) (*btcec.Signature, error) {

	return p.digestSigner.SignMessage(p.keyDesc, message, doubleHash)
}

func (p *PubKeyDigestSigner) SignDigestCompact(digest [32]byte) ([]byte,
	error) {

	return p.digestSigner.SignDigestCompact(p.keyDesc, digest)
}

type PrivKeyDigestSigner struct {
	PrivKey *btcec.PrivateKey
}

func (p *PrivKeyDigestSigner) PubKey() *btcec.PublicKey {
	return p.PrivKey.PubKey()
}

func (p *PrivKeyDigestSigner) SignMessage(msg []byte,
	doubleHash bool) (*btcec.Signature, error) {

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return p.PrivKey.Sign(digest)
}

func (p *PrivKeyDigestSigner) SignDigestCompact(digest [32]byte) ([]byte,
	error) {

	return btcec.SignCompact(btcec.S256(), p.PrivKey, digest[:], true)
}

var _ SingleKeyDigestSigner = (*PubKeyDigestSigner)(nil)
var _ SingleKeyDigestSigner = (*PrivKeyDigestSigner)(nil)
