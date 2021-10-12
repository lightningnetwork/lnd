package keychain

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func NewPubKeyMessageSigner(pubKey *btcec.PublicKey, keyLoc KeyLocator,
	signer MessageSignerRing) *PubKeyMessageSigner {

	return &PubKeyMessageSigner{
		pubKey:       pubKey,
		keyLoc:       keyLoc,
		digestSigner: signer,
	}
}

type PubKeyMessageSigner struct {
	pubKey       *btcec.PublicKey
	keyLoc       KeyLocator
	digestSigner MessageSignerRing
}

func (p *PubKeyMessageSigner) PubKey() *btcec.PublicKey {
	return p.pubKey
}

func (p *PubKeyMessageSigner) KeyLocator() KeyLocator {
	return p.keyLoc
}

func (p *PubKeyMessageSigner) SignMessage(message []byte,
	doubleHash bool) (*btcec.Signature, error) {

	return p.digestSigner.SignMessage(p.keyLoc, message, doubleHash)
}

func (p *PubKeyMessageSigner) SignMessageCompact(msg []byte,
	doubleHash bool) ([]byte, error) {

	return p.digestSigner.SignMessageCompact(p.keyLoc, msg, doubleHash)
}

func NewPrivKeyMessageSigner(privKey *btcec.PrivateKey,
	keyLoc KeyLocator) *PrivKeyMessageSigner {

	return &PrivKeyMessageSigner{
		privKey: privKey,
		keyLoc:  keyLoc,
	}
}

type PrivKeyMessageSigner struct {
	keyLoc  KeyLocator
	privKey *btcec.PrivateKey
}

func (p *PrivKeyMessageSigner) PubKey() *btcec.PublicKey {
	return p.privKey.PubKey()
}

func (p *PrivKeyMessageSigner) KeyLocator() KeyLocator {
	return p.keyLoc
}

func (p *PrivKeyMessageSigner) SignMessage(msg []byte,
	doubleHash bool) (*btcec.Signature, error) {

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return p.privKey.Sign(digest)
}

func (p *PrivKeyMessageSigner) SignMessageCompact(msg []byte,
	doubleHash bool) ([]byte, error) {

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return btcec.SignCompact(btcec.S256(), p.privKey, digest, true)
}

var _ SingleKeyMessageSigner = (*PubKeyMessageSigner)(nil)
var _ SingleKeyMessageSigner = (*PrivKeyMessageSigner)(nil)
