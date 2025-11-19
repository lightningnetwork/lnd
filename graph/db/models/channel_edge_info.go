package models

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ChannelEdgeInfo represents a fully authenticated channel along with all its
// unique attributes. Once an authenticated channel announcement has been
// processed on the network, then an instance of ChannelEdgeInfo encapsulating
// the channels attributes is stored. The other portions relevant to routing
// policy of a channel are stored within a ChannelEdgePolicy for each direction
// of the channel.
type ChannelEdgeInfo struct {
	// Version is the gossip version that this channel was advertised on.
	Version lnwire.GossipVersion

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// ChainHash is the hash that uniquely identifies the chain that this
	// channel was opened within.
	ChainHash chainhash.Hash

	// NodeKey1Bytes is the raw public key of the first node.
	NodeKey1Bytes route.Vertex

	// NodeKey2Bytes is the raw public key of the second node.
	NodeKey2Bytes route.Vertex

	// BitcoinKey1Bytes is the raw public key of the first node.
	BitcoinKey1Bytes fn.Option[route.Vertex]

	// BitcoinKey2Bytes is the raw public key of the second node.
	BitcoinKey2Bytes fn.Option[route.Vertex]

	// Features is the list of protocol features supported by this channel
	// edge.
	Features *lnwire.FeatureVector

	// AuthProof is the authentication proof for this channel. This proof
	// contains a set of signatures binding four identities, which attests
	// to the legitimacy of the advertised channel.
	AuthProof *ChannelAuthProof

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount

	// FundingScript holds the script of the channel's funding transaction.
	//
	// NOTE: this is not currently persisted and so will not be present if
	// the edge object is loaded from the database.
	FundingScript fn.Option[[]byte]

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// EdgeModifier is a functional option that modifies a ChannelEdgeInfo.
type EdgeModifier func(*ChannelEdgeInfo)

// WithChannelPoint sets the channel point (funding outpoint) on the edge.
func WithChannelPoint(cp wire.OutPoint) EdgeModifier {
	return func(e *ChannelEdgeInfo) {
		e.ChannelPoint = cp
	}
}

// WithFeatures sets the feature vector on the edge.
func WithFeatures(f *lnwire.RawFeatureVector) EdgeModifier {
	return func(e *ChannelEdgeInfo) {
		e.Features = lnwire.NewFeatureVector(f, lnwire.Features)
	}
}

// WithCapacity sets the capacity on the edge.
func WithCapacity(c btcutil.Amount) EdgeModifier {
	return func(e *ChannelEdgeInfo) {
		e.Capacity = c
	}
}

// WithChanProof sets the authentication proof on the edge.
func WithChanProof(proof *ChannelAuthProof) EdgeModifier {
	return func(e *ChannelEdgeInfo) {
		e.AuthProof = proof
	}
}

// WithFundingScript sets the funding script on the edge.
func WithFundingScript(script []byte) EdgeModifier {
	return func(e *ChannelEdgeInfo) {
		e.FundingScript = fn.Some(script)
	}
}

// ChannelV1Fields contains the fields that are specific to v1 channel
// announcements.
type ChannelV1Fields struct {
	// BitcoinKey1Bytes is the raw public key of the first node.
	BitcoinKey1Bytes route.Vertex

	// BitcoinKey2Bytes is the raw public key of the second node.
	BitcoinKey2Bytes route.Vertex

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// NewV1Channel creates a new ChannelEdgeInfo for a v1 channel announcement.
// It takes the required fields for all channels (chanID, chainHash, node keys)
// and v1-specific fields, along with optional modifiers for setting additional
// fields like capacity, channel point, features, and auth proof.
//
// The constructor validates that if an AuthProof is provided via modifiers, its
// version matches the channel version (v1).
func NewV1Channel(chanID uint64, chainHash chainhash.Hash, node1,
	node2 route.Vertex, v1Fields *ChannelV1Fields,
	opts ...EdgeModifier) (*ChannelEdgeInfo, error) {

	edge := &ChannelEdgeInfo{
		Version:          lnwire.GossipVersion1,
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: fn.Some(v1Fields.BitcoinKey1Bytes),
		BitcoinKey2Bytes: fn.Some(v1Fields.BitcoinKey2Bytes),
		ChannelID:        chanID,
		ChainHash:        chainHash,
		Features:         lnwire.EmptyFeatureVector(),
		ExtraOpaqueData:  v1Fields.ExtraOpaqueData,
	}

	for _, opt := range opts {
		opt(edge)
	}

	// Validate some fields after the options have been applied.
	if edge.AuthProof != nil && edge.AuthProof.Version != edge.Version {
		return nil, fmt.Errorf("channel auth proof version %d does "+
			"not match channel version %d", edge.AuthProof.Version,
			edge.Version)
	}

	return edge, nil
}

// NodeKey1 is the identity public key of the "first" node that was involved in
// the creation of this channel. A node is considered "first" if the
// lexicographical ordering the its serialized public key is "smaller" than
// that of the other node involved in channel creation.
func (c *ChannelEdgeInfo) NodeKey1() (*btcec.PublicKey, error) {
	return btcec.ParsePubKey(c.NodeKey1Bytes[:])
}

// NodeKey2 is the identity public key of the "second" node that was involved in
// the creation of this channel. A node is considered "second" if the
// lexicographical ordering the its serialized public key is "larger" than that
// of the other node involved in channel creation.
func (c *ChannelEdgeInfo) NodeKey2() (*btcec.PublicKey, error) {
	return btcec.ParsePubKey(c.NodeKey2Bytes[:])
}

// OtherNodeKeyBytes returns the node key bytes of the other end of the channel.
func (c *ChannelEdgeInfo) OtherNodeKeyBytes(thisNodeKey []byte) (
	route.Vertex, error) {

	switch {
	case bytes.Equal(c.NodeKey1Bytes[:], thisNodeKey):
		return c.NodeKey2Bytes, nil
	case bytes.Equal(c.NodeKey2Bytes[:], thisNodeKey):
		return c.NodeKey1Bytes, nil
	default:
		return route.Vertex{}, fmt.Errorf("node not participating in " +
			"this channel")
	}
}

// FundingPKScript returns the funding output's pkScript for the channel.
func (c *ChannelEdgeInfo) FundingPKScript() ([]byte, error) {
	switch c.Version {
	case lnwire.GossipVersion1:
		btc1Key, err := c.BitcoinKey1Bytes.UnwrapOrErr(
			fmt.Errorf("missing bitcoin key 1"),
		)
		if err != nil {
			return nil, err
		}
		btc2Key, err := c.BitcoinKey2Bytes.UnwrapOrErr(
			fmt.Errorf("missing bitcoin key 2"),
		)
		if err != nil {
			return nil, err
		}

		witnessScript, err := input.GenMultiSigScript(
			btc1Key[:], btc2Key[:],
		)
		if err != nil {
			return nil, err
		}

		return input.WitnessScriptHash(witnessScript)

	default:
		return nil, fmt.Errorf("unsupported channel version: %d",
			c.Version)
	}
}

// ToChannelAnnouncement converts the ChannelEdgeInfo to a
// lnwire.ChannelAnnouncement1 message. Returns an error if AuthProof is nil
// or if the version is not v1.
func (c *ChannelEdgeInfo) ToChannelAnnouncement() (
	*lnwire.ChannelAnnouncement1, error) {

	// We currently only support v1 channel announcements.
	if c.Version != lnwire.GossipVersion1 {
		return nil, fmt.Errorf("unsupported channel version: %d",
			c.Version)
	}

	// If there's no auth proof, we can't create a full channel
	// announcement.
	if c.AuthProof == nil {
		return nil, fmt.Errorf("cannot create channel announcement " +
			"without auth proof")
	}

	btc1, err := c.BitcoinKey1Bytes.UnwrapOrErr(
		fmt.Errorf("bitcoin key 1 missing for v1 channel announcement"),
	)
	if err != nil {
		return nil, err
	}

	btc2, err := c.BitcoinKey2Bytes.UnwrapOrErr(
		fmt.Errorf("bitcoin key 2 missing for v1 channel announcement"),
	)
	if err != nil {
		return nil, err
	}

	chanID := lnwire.NewShortChanIDFromInt(c.ChannelID)
	chanAnn := &lnwire.ChannelAnnouncement1{
		ShortChannelID:  chanID,
		NodeID1:         c.NodeKey1Bytes,
		NodeID2:         c.NodeKey2Bytes,
		ChainHash:       c.ChainHash,
		BitcoinKey1:     btc1,
		BitcoinKey2:     btc2,
		Features:        c.Features.RawFeatureVector,
		ExtraOpaqueData: c.ExtraOpaqueData,
	}

	chanAnn.NodeSig1, err = lnwire.NewSigFromECDSARawSignature(
		c.AuthProof.NodeSig1(),
	)
	if err != nil {
		return nil, err
	}

	chanAnn.NodeSig2, err = lnwire.NewSigFromECDSARawSignature(
		c.AuthProof.NodeSig2(),
	)
	if err != nil {
		return nil, err
	}

	chanAnn.BitcoinSig1, err = lnwire.NewSigFromECDSARawSignature(
		c.AuthProof.BitcoinSig1(),
	)
	if err != nil {
		return nil, err
	}

	chanAnn.BitcoinSig2, err = lnwire.NewSigFromECDSARawSignature(
		c.AuthProof.BitcoinSig2(),
	)
	if err != nil {
		return nil, err
	}

	return chanAnn, nil
}
