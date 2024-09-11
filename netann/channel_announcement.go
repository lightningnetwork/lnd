package netann

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CreateChanAnnouncement is a helper function which creates all channel
// announcements given the necessary channel related database items. This
// function is used to transform out database structs into the corresponding wire
// structs for announcing new channels to other peers, or simply syncing up a
// peer's initial routing table upon connect.
func CreateChanAnnouncement(chanProof *models.ChannelAuthProof,
	chanInfo *models.ChannelEdgeInfo,
	e1, e2 *models.ChannelEdgePolicy) (*lnwire.ChannelAnnouncement1,
	*lnwire.ChannelUpdate1, *lnwire.ChannelUpdate1, error) {

	// First, using the parameters of the channel, along with the channel
	// authentication chanProof, we'll create re-create the original
	// authenticated channel announcement.
	chanID := lnwire.NewShortChanIDFromInt(chanInfo.ChannelID)
	chanAnn := &lnwire.ChannelAnnouncement1{
		ShortChannelID:  chanID,
		NodeID1:         chanInfo.NodeKey1Bytes,
		NodeID2:         chanInfo.NodeKey2Bytes,
		ChainHash:       chanInfo.ChainHash,
		BitcoinKey1:     chanInfo.BitcoinKey1Bytes,
		BitcoinKey2:     chanInfo.BitcoinKey2Bytes,
		Features:        lnwire.NewRawFeatureVector(),
		ExtraOpaqueData: chanInfo.ExtraOpaqueData,
	}

	err := chanAnn.Features.Decode(bytes.NewReader(chanInfo.Features))
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.BitcoinSig1, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.BitcoinSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.BitcoinSig2, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.BitcoinSig2Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig1, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.NodeSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig2, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.NodeSig2Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// We'll unconditionally queue the channel's existence chanProof as it
	// will need to be processed before either of the channel update
	// networkMsgs.

	// Since it's up to a node's policy as to whether they advertise the
	// edge in a direction, we don't create an advertisement if the edge is
	// nil.
	var edge1Ann, edge2Ann *lnwire.ChannelUpdate1
	if e1 != nil {
		edge1Ann, err = ChannelUpdateFromEdge(chanInfo, e1)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if e2 != nil {
		edge2Ann, err = ChannelUpdateFromEdge(chanInfo, e2)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return chanAnn, edge1Ann, edge2Ann, nil
}

// ValidateChannelAnn validates the channel announcement message and checks
// that node signatures covers the announcement message, and that the bitcoin
// signatures covers the node keys.
func ValidateChannelAnn(a *lnwire.ChannelAnnouncement1) error {
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
	bitcoinSig1, err := a.BitcoinSig1.ToSignature()
	if err != nil {
		return err
	}
	bitcoinKey1, err := btcec.ParsePubKey(a.BitcoinKey1[:])
	if err != nil {
		return err
	}
	if !bitcoinSig1.Verify(dataHash, bitcoinKey1) {
		return errors.New("can't verify first bitcoin signature")
	}

	// If that checks out, then we'll verify that the second bitcoin
	// signature is a valid signature of the bitcoin public key over hash
	// digest as well.
	bitcoinSig2, err := a.BitcoinSig2.ToSignature()
	if err != nil {
		return err
	}
	bitcoinKey2, err := btcec.ParsePubKey(a.BitcoinKey2[:])
	if err != nil {
		return err
	}
	if !bitcoinSig2.Verify(dataHash, bitcoinKey2) {
		return errors.New("can't verify second bitcoin signature")
	}

	// Both node signatures attached should indeed be a valid signature
	// over the selected digest of the channel announcement signature.
	nodeSig1, err := a.NodeSig1.ToSignature()
	if err != nil {
		return err
	}
	nodeKey1, err := btcec.ParsePubKey(a.NodeID1[:])
	if err != nil {
		return err
	}
	if !nodeSig1.Verify(dataHash, nodeKey1) {
		return errors.New("can't verify data in first node signature")
	}

	nodeSig2, err := a.NodeSig2.ToSignature()
	if err != nil {
		return err
	}
	nodeKey2, err := btcec.ParsePubKey(a.NodeID2[:])
	if err != nil {
		return err
	}
	if !nodeSig2.Verify(dataHash, nodeKey2) {
		return errors.New("can't verify data in second node signature")
	}

	return nil
}
