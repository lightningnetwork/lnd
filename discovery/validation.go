package discovery

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// validateChannelAnn validates the channel announcement message and checks
// that node signatures covers the announcement message, and that the bitcoin
// signatures covers the node keys.
func (d *AuthenticatedGossiper) validateChannelAnn(a *lnwire.ChannelAnnouncement) error {
	// First we'll verify that the passed bitcoin key signature is indeed a
	// signature over the digest of the node signature.
	sigHash := chainhash.DoubleHashB(a.NodeID1.SerializeCompressed())
	if !a.BitcoinSig1.Verify(sigHash, copyPubKey(a.BitcoinKey1)) {
		return errors.New("can't verify first bitcoin signature")
	}

	// If that checks out, then we'll verify that the second bitcoin
	// signature is a valid signature of the bitcoin public key over the
	// second node signature.
	sigHash = chainhash.DoubleHashB(a.NodeID2.SerializeCompressed())
	if !a.BitcoinSig2.Verify(sigHash, copyPubKey(a.BitcoinKey2)) {
		return errors.New("can't verify second bitcoin signature")
	}

	// With the first two bitcoin signatures verified, we'll reconstruct
	// the original digest of the channel announcement message.
	data, err := a.DataToSign()
	if err != nil {
		return err
	}
	dataHash := chainhash.DoubleHashB(data)

	// Both node signatures attached should indeed be a valid signature
	// over the selected digest of the channel announcement signature.
	if !a.NodeSig1.Verify(dataHash, copyPubKey(a.NodeID1)) {
		return errors.New("can't verify data in first node signature")
	}
	if !a.NodeSig2.Verify(dataHash, copyPubKey(a.NodeID2)) {
		return errors.New("can't verify data in second node signature")
	}

	return nil

}

// validateNodeAnn validates the node announcement by ensuring that the
// attached signature is needed a signature of the node announcement under the
// specified node public key.
func (d *AuthenticatedGossiper) validateNodeAnn(a *lnwire.NodeAnnouncement) error {
	// Reconstruct the data of announcement which should be covered by the
	// signature so we can verify the signature shortly below
	data, err := a.DataToSign()
	if err != nil {
		return err
	}

	// Finally ensure that the passed signature is valid, if not we'll
	// return an error so this node announcement can be rejected.
	dataHash := chainhash.DoubleHashB(data)
	if !a.Signature.Verify(dataHash, copyPubKey(a.NodeID)) {
		return errors.New("signature on node announcement is invalid")
	}

	return nil
}

// validateChannelUpdateAnn validates the channel update announcement by
// checking that the included signature covers he announcement and has been
// signed by the node's private key.
func (d *AuthenticatedGossiper) validateChannelUpdateAnn(pubKey *btcec.PublicKey,
	a *lnwire.ChannelUpdateAnnouncement) error {

	data, err := a.DataToSign()
	if err != nil {
		return errors.Errorf("unable to reconstruct message: %v", err)
	}
	dataHash := chainhash.DoubleHashB(data)

	if !a.Signature.Verify(dataHash, copyPubKey(pubKey)) {
		return errors.Errorf("verification of channel updates "+
			"failed chan_id=%v", a.ShortChannelID.ToUint64())
	}

	return nil
}
