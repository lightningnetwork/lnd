package keychain

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func NewPubKeyMessageSigner(keyDesc KeyDescriptor,
	signer MessageSignerRing) *PubKeyMessageSigner {

	return &PubKeyMessageSigner{
		keyDesc:      keyDesc,
		digestSigner: signer,
	}
}

type PubKeyMessageSigner struct {
	keyDesc      KeyDescriptor
	digestSigner MessageSignerRing
}

func (p *PubKeyMessageSigner) PubKey() *btcec.PublicKey {
	return p.keyDesc.PubKey
}

func (p *PubKeyMessageSigner) SignMessage(msg []byte) (*btcec.Signature, error) {
	return p.digestSigner.SignMessage(p.keyDesc, msg)
}

func (p *PubKeyMessageSigner) SignMessageCompact(msg []byte) ([]byte, error) {
	return p.digestSigner.SignMessageCompact(p.keyDesc, msg)
}

type PrivKeyMessageSigner struct {
	PrivKey *btcec.PrivateKey
}

func (p *PrivKeyMessageSigner) PubKey() *btcec.PublicKey {
	return p.PrivKey.PubKey()
}

func (p *PrivKeyMessageSigner) SignMessage(msg []byte) (*btcec.Signature,
	error) {

	digest := chainhash.DoubleHashB(msg)
	return p.PrivKey.Sign(digest)
}

func (p *PrivKeyMessageSigner) SignMessageCompact(msg []byte) ([]byte, error) {
	digest := chainhash.DoubleHashB(msg)
	return btcec.SignCompact(btcec.S256(), p.PrivKey, digest, true)
}

var _ SingleKeyMessageSigner = (*PubKeyMessageSigner)(nil)
var _ SingleKeyMessageSigner = (*PrivKeyMessageSigner)(nil)
