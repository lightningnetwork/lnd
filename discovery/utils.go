package discovery

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
)

// createChanAnnouncement is a helper function which creates all channel
// announcements given the necessary channel related database items. This
// function is used to transform out databse structs into the coresponding wire
// sturcts for announcing new channels to other peers, or simply syncing up a
// peer's initial routing table upon connect.
func createChanAnnouncement(chanProof *channeldb.ChannelAuthProof,
	chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) (*lnwire.ChannelAnnouncement,
	*lnwire.ChannelUpdate, *lnwire.ChannelUpdate) {

	// First, using the parameters of the channel, along with the channel
	// authentication chanProof, we'll create re-create the original
	// authenticated channel announcement.
	chanID := lnwire.NewShortChanIDFromInt(chanInfo.ChannelID)
	chanAnn := &lnwire.ChannelAnnouncement{
		NodeSig1:       chanProof.NodeSig1,
		NodeSig2:       chanProof.NodeSig2,
		ShortChannelID: chanID,
		BitcoinSig1:    chanProof.BitcoinSig1,
		BitcoinSig2:    chanProof.BitcoinSig2,
		NodeID1:        chanInfo.NodeKey1,
		NodeID2:        chanInfo.NodeKey2,
		ChainHash:      chanInfo.ChainHash,
		BitcoinKey1:    chanInfo.BitcoinKey1,
		Features:       lnwire.NewRawFeatureVector(),
		BitcoinKey2:    chanInfo.BitcoinKey2,
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
			Signature:       e1.Signature,
			ChainHash:       chanInfo.ChainHash,
			ShortChannelID:  chanID,
			Timestamp:       uint32(e1.LastUpdate.Unix()),
			Flags:           e1.Flags,
			TimeLockDelta:   e1.TimeLockDelta,
			HtlcMinimumMsat: e1.MinHTLC,
			BaseFee:         uint32(e1.FeeBaseMSat),
			FeeRate:         uint32(e1.FeeProportionalMillionths),
		}
	}
	if e2 != nil {
		edge2Ann = &lnwire.ChannelUpdate{
			Signature:       e2.Signature,
			ChainHash:       chanInfo.ChainHash,
			ShortChannelID:  chanID,
			Timestamp:       uint32(e2.LastUpdate.Unix()),
			Flags:           e2.Flags,
			TimeLockDelta:   e2.TimeLockDelta,
			HtlcMinimumMsat: e2.MinHTLC,
			BaseFee:         uint32(e2.FeeBaseMSat),
			FeeRate:         uint32(e2.FeeProportionalMillionths),
		}
	}

	return chanAnn, edge1Ann, edge2Ann
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
