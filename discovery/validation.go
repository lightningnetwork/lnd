package discovery

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// validateChannelAnn validates the channel announcement message and checks
// that node signatures contains the announcement message, and that the
// bitcoin signatures contains the node keys.
func (d *Discovery) validateChannelAnn(a *lnwire.ChannelAnnouncement) error {
	sigHash := chainhash.DoubleHashB(a.NodeID1.SerializeCompressed())
	if !a.BitcoinSig1.Verify(sigHash, copyPubKey(a.BitcoinKey1)) {
		return errors.New("can't verify first bitcoin signature")
	}

	sigHash = chainhash.DoubleHashB(a.NodeID2.SerializeCompressed())
	if !a.BitcoinSig2.Verify(sigHash, copyPubKey(a.BitcoinKey2)) {
		return errors.New("can't verify second bitcoin signature")
	}

	// Get the data of announcement which should be encapsulated in
	// signature and then check it.
	data, err := a.DataToSign()
	if err != nil {
		return err
	}
	dataHash := chainhash.DoubleHashB(data)

	if !a.NodeSig1.Verify(dataHash, copyPubKey(a.NodeID1)) {
		return errors.New("can't verify data in first node signature")
	}

	if !a.NodeSig2.Verify(dataHash, copyPubKey(a.NodeID2)) {
		return errors.New("can't verify data in second node signature")
	}
	return nil

}

// validateNodeAnn validates the node announcement by checking that the
// the signature corresponds to the node key and have been created with
// announcement data.
func (d *Discovery) validateNodeAnn(a *lnwire.NodeAnnouncement) error {

	// Get the data of announcement which should be encapsulated in
	// signature and then check it.
	data, err := a.DataToSign()
	if err != nil {
		return err
	}

	dataHash := chainhash.DoubleHashB(data)
	if !a.Signature.Verify(dataHash, copyPubKey(a.NodeID)) {
		return errors.New("can't check the node annoucement signature")
	}

	return nil
}

// validateChannelUpdateAnn validates the channel update announcement by
// checking that the the signature corresponds to the node key and have been
// created with announcement data.
func (d *Discovery) validateChannelUpdateAnn(pubKey *btcec.PublicKey,
	a *lnwire.ChannelUpdateAnnouncement) error {

	// Get the data of announcement which should be encapsulated in
	// signature and then check it.
	data, err := a.DataToSign()
	if err != nil {
		return errors.Errorf("can't retrieve data to sign: %v", err)
	}
	dataHash := chainhash.DoubleHashB(data)

	if !a.Signature.Verify(dataHash, copyPubKey(pubKey)) {
		return errors.Errorf("verification of channel updates "+
			"failed chan_id=%v", a.ShortChannelID)
	}

	return nil
}
