package discovery

import (
	"bytes"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// ValidateChannelAnn validates the channel announcement message and checks
// that node signatures covers the announcement message, and that the bitcoin
// signatures covers the node keys.
func ValidateChannelAnn(a *lnwire.ChannelAnnouncement) error {
	// First, we'll compute the digest (h) which is to be signed by each of
	// the keys included within the node announcement message. This hash
	// digest includes all the keys, so the (up to 4 signatures) will
	// attest to the validity of each of the keys.
	data, err := a.DataToSign()
	if err != nil {
		return err
	}
	dataHash := chainhash.DoubleHashB(data)

	// First we'll verify that the passed bitcoin key signature is indeed a
	// signature over the computed hash digest.
	if !a.BitcoinSig1.Verify(dataHash, copyPubKey(a.BitcoinKey1)) {
		return errors.New("can't verify first bitcoin signature")
	}

	// If that checks out, then we'll verify that the second bitcoin
	// signature is a valid signature of the bitcoin public key over hash
	// digest as well.
	if !a.BitcoinSig2.Verify(dataHash, copyPubKey(a.BitcoinKey2)) {
		return errors.New("can't verify second bitcoin signature")
	}

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

// ValidateNodeAnn validates the node announcement by ensuring that the
// attached signature is needed a signature of the node announcement under the
// specified node public key.
func ValidateNodeAnn(a *lnwire.NodeAnnouncement) error {
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
		var msgBuf bytes.Buffer
		if _, err := lnwire.WriteMessage(&msgBuf, a, 0); err != nil {
			return err
		}

		return errors.Errorf("signature on NodeAnnouncement(%x) is "+
			"invalid: %x", a.NodeID.SerializeCompressed(),
			msgBuf.Bytes())
	}

	return nil
}

// ValidateChannelUpdateAnn validates the channel update announcement by
// checking that the included signature covers he announcement and has been
// signed by the node's private key.
func ValidateChannelUpdateAnn(pubKey *btcec.PublicKey,
	a *lnwire.ChannelUpdate) error {

	data, err := a.DataToSign()
	if err != nil {
		return errors.Errorf("unable to reconstruct message: %v", err)
	}
	dataHash := chainhash.DoubleHashB(data)

	if !a.Signature.Verify(dataHash, copyPubKey(pubKey)) {
		return errors.Errorf("invalid signature for channel "+
			"update %v", spew.Sdump(a))
	}

	return nil
}
