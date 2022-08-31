package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// MusigPartialSig...
//
// TODO(roasbeef): move to wire package?
type MusigPartialSig struct {
	sig *musig2.PartialSignature

	signerNonce [musig2.PubNonceSize]byte

	combinedNonce [musig2.PubNonceSize]byte

	signerKeys []*btcec.PublicKey
}

// NewMusigPartialSig...
//
// TODO(roasbeef): need version that lets bind the rest later?
func NewMusigPartialSig(sig *musig2.PartialSignature,
	signerNonce, combinedNonce [musig2.PubNonceSize]byte,
	signerKeys []*btcec.PublicKey) *MusigPartialSig {

	return &MusigPartialSig{
		sig:           sig,
		signerNonce:   signerNonce,
		combinedNonce: combinedNonce,
		signerKeys:    signerKeys,
	}
}

// Serialize serializes the musig2 partial signature. The serializing includes
// the combined nonce _and_ the partial signature. The final signature is
// always 64 bytes in length.
func (p *MusigPartialSig) Serialize() []byte {
	var rawSig [schnorr.SignatureSize]byte

	// For the signature, we'll encode only the x-coordinate of the
	// combined nonce point. To do this we'll need to convert the R point
	// in the sig to jacobian coordinate, and then extract the x-coord from
	// that.
	//
	// TODO(roasbeef): test, or can recompute b, then arrive at the
	// combined nonce, given: combinedNonce, combinedKey, msg
	var nonceJ btcec.JacobianPoint
	p.sig.R.AsJacobian(&nonceJ)
	nonceJ.ToAffine()

	nonceX := &nonceJ.X

	nonceX.PutBytesUnchecked(rawSig[:])
	p.sig.S.PutBytesUnchecked(rawSig[32:])

	return rawSig[:]
}

// TODO(roasbeef): parse method, can recompute the nonce like above?

// Verify...
func (p *MusigPartialSig) Verify(msg []byte, pub *btcec.PublicKey) bool {
	var m [32]byte
	copy(m[:], msg)

	// TODO(roasbeef): need diff nonce here??

	return p.sig.Verify(
		p.signerNonce, p.combinedNonce, p.signerKeys, pub, m,
		musig2.WithSortedKeys(), musig2.WithBip86SignTweak(),
	)
}
