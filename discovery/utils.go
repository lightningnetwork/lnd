package discovery

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CreateChanAnnouncement is a helper function which creates all channel
// announcements given the necessary channel related database items. This
// function is used to transform out database structs into the corresponding wire
// structs for announcing new channels to other peers, or simply syncing up a
// peer's initial routing table upon connect.
func CreateChanAnnouncement(chanProof *channeldb.ChannelAuthProof,
	chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) (*lnwire.ChannelAnnouncement,
	*lnwire.ChannelUpdate, *lnwire.ChannelUpdate, error) {

	// First, using the parameters of the channel, along with the channel
	// authentication chanProof, we'll create re-create the original
	// authenticated channel announcement.
	chanID := lnwire.NewShortChanIDFromInt(chanInfo.ChannelID)
	chanAnn := &lnwire.ChannelAnnouncement{
		ShortChannelID:  chanID,
		NodeID1:         chanInfo.NodeKey1Bytes,
		NodeID2:         chanInfo.NodeKey2Bytes,
		ChainHash:       chanInfo.ChainHash,
		BitcoinKey1:     chanInfo.BitcoinKey1Bytes,
		BitcoinKey2:     chanInfo.BitcoinKey2Bytes,
		Features:        lnwire.NewRawFeatureVector(),
		ExtraOpaqueData: chanInfo.ExtraOpaqueData,
	}

	var err error
	chanAnn.BitcoinSig1, err = lnwire.NewSigFromRawSignature(
		chanProof.BitcoinSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.BitcoinSig2, err = lnwire.NewSigFromRawSignature(
		chanProof.BitcoinSig2Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig1, err = lnwire.NewSigFromRawSignature(
		chanProof.NodeSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig2, err = lnwire.NewSigFromRawSignature(
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
	var edge1Ann, edge2Ann *lnwire.ChannelUpdate
	if e1 != nil {
		edge1Ann = &lnwire.ChannelUpdate{
			ChainHash:       chanInfo.ChainHash,
			ShortChannelID:  chanID,
			Timestamp:       uint32(e1.LastUpdate.Unix()),
			Flags:           e1.Flags,
			TimeLockDelta:   e1.TimeLockDelta,
			HtlcMinimumMsat: e1.MinHTLC,
			BaseFee:         uint32(e1.FeeBaseMSat),
			FeeRate:         uint32(e1.FeeProportionalMillionths),
			ExtraOpaqueData: e1.ExtraOpaqueData,
		}
		edge1Ann.Signature, err = lnwire.NewSigFromRawSignature(e1.SigBytes)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if e2 != nil {
		edge2Ann = &lnwire.ChannelUpdate{
			ChainHash:       chanInfo.ChainHash,
			ShortChannelID:  chanID,
			Timestamp:       uint32(e2.LastUpdate.Unix()),
			Flags:           e2.Flags,
			TimeLockDelta:   e2.TimeLockDelta,
			HtlcMinimumMsat: e2.MinHTLC,
			BaseFee:         uint32(e2.FeeBaseMSat),
			FeeRate:         uint32(e2.FeeProportionalMillionths),
			ExtraOpaqueData: e2.ExtraOpaqueData,
		}
		edge2Ann.Signature, err = lnwire.NewSigFromRawSignature(e2.SigBytes)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return chanAnn, edge1Ann, edge2Ann, nil
}

// copyPubKey performs a copy of the target public key, setting a fresh curve
// parameter during the process.
func copyPubKey(pub *btcec.PublicKey) *btcec.PublicKey {
	return &btcec.PublicKey{
		Curve: btcec.S256(),
		X:     pub.X,
		Y:     pub.Y,
	}
}

// SignAnnouncement is a helper function which is used to sign any outgoing
// channel node node announcement messages.
func SignAnnouncement(signer lnwallet.MessageSigner, pubKey *btcec.PublicKey,
	msg lnwire.Message) (*btcec.Signature, error) {

	var (
		data []byte
		err  error
	)

	switch m := msg.(type) {
	case *lnwire.ChannelAnnouncement:
		data, err = m.DataToSign()
	case *lnwire.ChannelUpdate:
		data, err = m.DataToSign()
	case *lnwire.NodeAnnouncement:
		data, err = m.DataToSign()
	default:
		return nil, errors.New("can't sign message " +
			"of this format")
	}
	if err != nil {
		return nil, errors.Errorf("unable to get data to sign: %v", err)
	}

	return signer.SignMessage(pubKey, data)
}
