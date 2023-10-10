package discovery

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
)

// ChanAnn is an interface representing the info that the handleChanAnnouncement
// method needs to know about a channel announcement. This abstraction allows
// the handleChanAnnouncement method to work for multiple types of channel
// announcements.
type ChanAnn interface {
	// SCID returns the short channel ID of the channel being announced.
	SCID() lnwire.ShortChannelID

	// ChainHash returns the hash identifying the chain that the channel
	// was opened on.
	ChainHash() chainhash.Hash

	// Name returns the underlying lnwire message name.
	Name() string

	// Msg returns the underlying lnwire.Message.
	Msg() lnwire.Message

	// Validate validates the message.
	Validate() error

	// Create constructs a new ChanAnn from the given edge info, channel
	// proof and edge policies.
	Create(chanProof *channeldb.ChannelAuthProof,
		chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) (ChanAnn,
		*lnwire.ChannelUpdate, *lnwire.ChannelUpdate, error)

	// BuildProof converts the lnwire.Message into a
	// channeldb.ChannelAuthProof.
	BuildProof() *channeldb.ChannelAuthProof

	// BuildEdgeInfo converts the lnwire.Message into a
	// channeldb.ChannelEdgeInfo.
	BuildEdgeInfo() (*channeldb.ChannelEdgeInfo, error)

	lnwire.Message
}

// chanAnn is an implementation of the ChanAnn interface which represents the
// lnwire.ChannelAnnouncement message used for P2WSH channels.
type chanAnn struct {
	*lnwire.ChannelAnnouncement
}

// SCID returns the short channel ID of the channel being announced.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) SCID() lnwire.ShortChannelID {
	return c.ShortChannelID
}

// ChainHash returns the hash identifying the chain that the channel was opened
// on.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) ChainHash() chainhash.Hash {
	return c.ChannelAnnouncement.ChainHash
}

// Name returns the underlying lnwire message name.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) Name() string {
	return "ChannelAnnouncement"
}

// Validate validates the message.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) Validate() error {
	return routing.ValidateChannelAnn(c.ChannelAnnouncement)
}

// Create constructs a new ChanAnn from the given edge info, channel
// proof and edge policies.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) Create(chanProof *channeldb.ChannelAuthProof,
	chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) (ChanAnn, *lnwire.ChannelUpdate,
	*lnwire.ChannelUpdate, error) {

	ann, update1, update2, err := netann.CreateChanAnnouncement(
		chanProof, chanInfo, e1, e2,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return &chanAnn{ann}, update1, update2, nil
}

// BuildProof converts the lnwire.Message into a channeldb.ChannelAuthProof.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) BuildProof() *channeldb.ChannelAuthProof {
	return &channeldb.ChannelAuthProof{
		NodeSig1Bytes:    c.NodeSig1.ToSignatureBytes(),
		NodeSig2Bytes:    c.NodeSig2.ToSignatureBytes(),
		BitcoinSig1Bytes: c.BitcoinSig1.ToSignatureBytes(),
		BitcoinSig2Bytes: c.BitcoinSig2.ToSignatureBytes(),
	}
}

// BuildEdgeInfo converts the lnwire.Message into a channeldb.ChannelEdgeInfo.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) BuildEdgeInfo() (*channeldb.ChannelEdgeInfo, error) {
	var featureBuf bytes.Buffer
	if err := c.Features.Encode(&featureBuf); err != nil {
		return nil, fmt.Errorf("unable to encode features: %v", err)
	}

	return &channeldb.ChannelEdgeInfo{
		ChannelID:        c.ShortChannelID.ToUint64(),
		ChainHash:        c.ChainHash(),
		NodeKey1Bytes:    c.NodeID1,
		NodeKey2Bytes:    c.NodeID2,
		BitcoinKey1Bytes: c.BitcoinKey1,
		BitcoinKey2Bytes: c.BitcoinKey2,
		Features:         featureBuf.Bytes(),
		ExtraOpaqueData:  c.ExtraOpaqueData,
	}, nil
}

// Msg returns the underlying lnwire.Message.
//
// NOTE: this is part of the ChanAnn interface.
func (c *chanAnn) Msg() lnwire.Message {
	return c.ChannelAnnouncement
}

var _ ChanAnn = (*chanAnn)(nil)
