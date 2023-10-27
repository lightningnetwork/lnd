package models

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelEdgeInfo1 represents a fully authenticated channel along with all its
// unique attributes. Once an authenticated channel announcement has been
// processed on the network, then an instance of ChannelEdgeInfo1 encapsulating
// the channels attributes is stored. The other portions relevant to routing
// policy of a channel are stored within a ChannelEdgePolicy1 for each direction
// of the channel.
type ChannelEdgeInfo1 struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// ChainHash is the hash that uniquely identifies the chain that this
	// channel was opened within.
	//
	// TODO(roasbeef): need to modify db keying for multi-chain
	//  * must add chain hash to prefix as well
	ChainHash chainhash.Hash

	// NodeKey1Bytes is the raw public key of the first node.
	NodeKey1Bytes [33]byte
	nodeKey1      *btcec.PublicKey

	// NodeKey2Bytes is the raw public key of the first node.
	NodeKey2Bytes [33]byte
	nodeKey2      *btcec.PublicKey

	// BitcoinKey1Bytes is the raw public key of the first node.
	BitcoinKey1Bytes [33]byte
	bitcoinKey1      *btcec.PublicKey

	// BitcoinKey2Bytes is the raw public key of the first node.
	BitcoinKey2Bytes [33]byte
	bitcoinKey2      *btcec.PublicKey

	// Features is an opaque byte slice that encodes the set of channel
	// specific features that this channel edge supports.
	Features []byte

	// AuthProof is the authentication proof for this channel. This proof
	// contains a set of signatures binding four identities, which attests
	// to the legitimacy of the advertised channel.
	AuthProof *ChannelAuthProof1

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// AddNodeKeys is a setter-like method that can be used to replace the set of
// keys for the target ChannelEdgeInfo1.
func (c *ChannelEdgeInfo1) AddNodeKeys(nodeKey1, nodeKey2, bitcoinKey1,
	bitcoinKey2 *btcec.PublicKey) {

	c.nodeKey1 = nodeKey1
	copy(c.NodeKey1Bytes[:], c.nodeKey1.SerializeCompressed())

	c.nodeKey2 = nodeKey2
	copy(c.NodeKey2Bytes[:], nodeKey2.SerializeCompressed())

	c.bitcoinKey1 = bitcoinKey1
	copy(c.BitcoinKey1Bytes[:], c.bitcoinKey1.SerializeCompressed())

	c.bitcoinKey2 = bitcoinKey2
	copy(c.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())
}

// NodeKey1 is the identity public key of the "first" node that was involved in
// the creation of this channel. A node is considered "first" if the
// lexicographical ordering the its serialized public key is "smaller" than
// that of the other node involved in channel creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo1) NodeKey1() (*btcec.PublicKey, error) {
	if c.nodeKey1 != nil {
		return c.nodeKey1, nil
	}

	key, err := btcec.ParsePubKey(c.NodeKey1Bytes[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey1 = key

	return key, nil
}

// NodeKey2 is the identity public key of the "second" node that was involved in
// the creation of this channel. A node is considered "second" if the
// lexicographical ordering the its serialized public key is "larger" than that
// of the other node involved in channel creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo1) NodeKey2() (*btcec.PublicKey, error) {
	if c.nodeKey2 != nil {
		return c.nodeKey2, nil
	}

	key, err := btcec.ParsePubKey(c.NodeKey2Bytes[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey2 = key

	return key, nil
}

// BitcoinKey1 is the Bitcoin multi-sig key belonging to the first node, that
// was involved in the funding transaction that originally created the channel
// that this struct represents.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo1) BitcoinKey1() (*btcec.PublicKey, error) {
	if c.bitcoinKey1 != nil {
		return c.bitcoinKey1, nil
	}

	key, err := btcec.ParsePubKey(c.BitcoinKey1Bytes[:])
	if err != nil {
		return nil, err
	}
	c.bitcoinKey1 = key

	return key, nil
}

// BitcoinKey2 is the Bitcoin multi-sig key belonging to the second node, that
// was involved in the funding transaction that originally created the channel
// that this struct represents.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo1) BitcoinKey2() (*btcec.PublicKey, error) {
	if c.bitcoinKey2 != nil {
		return c.bitcoinKey2, nil
	}

	key, err := btcec.ParsePubKey(c.BitcoinKey2Bytes[:])
	if err != nil {
		return nil, err
	}
	c.bitcoinKey2 = key

	return key, nil
}

// OtherNodeKeyBytes returns the node key bytes of the other end of the channel.
func (c *ChannelEdgeInfo1) OtherNodeKeyBytes(thisNodeKey []byte) (
	[33]byte, error) {

	switch {
	case bytes.Equal(c.NodeKey1Bytes[:], thisNodeKey):
		return c.NodeKey2Bytes, nil
	case bytes.Equal(c.NodeKey2Bytes[:], thisNodeKey):
		return c.NodeKey1Bytes, nil
	default:
		return [33]byte{}, fmt.Errorf("node not participating in " +
			"this channel")
	}
}

// Copy returns a copy of the ChannelEdgeInfo.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) Copy() ChannelEdgeInfo {
	return &ChannelEdgeInfo1{
		ChannelID:        c.ChannelID,
		ChainHash:        c.ChainHash,
		NodeKey1Bytes:    c.NodeKey1Bytes,
		NodeKey2Bytes:    c.NodeKey2Bytes,
		BitcoinKey1Bytes: c.BitcoinKey1Bytes,
		BitcoinKey2Bytes: c.BitcoinKey2Bytes,
		Features:         c.Features,
		AuthProof:        c.AuthProof,
		ChannelPoint:     c.ChannelPoint,
		Capacity:         c.Capacity,
		ExtraOpaqueData:  c.ExtraOpaqueData,
	}
}

// Node1Bytes returns bytes of the public key of node 1.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) Node1Bytes() [33]byte {
	return c.NodeKey1Bytes
}

// Node2Bytes returns bytes of the public key of node 2.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) Node2Bytes() [33]byte {
	return c.NodeKey2Bytes
}

// GetChainHash returns the hash of the genesis block of the chain that the edge
// is on.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) GetChainHash() chainhash.Hash {
	return c.ChainHash
}

// GetChanID returns the channel ID.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) GetChanID() uint64 {
	return c.ChannelID
}

// GetAuthProof returns the ChannelAuthProof for the edge.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) GetAuthProof() ChannelAuthProof {
	// Cant just return AuthProof cause you then run into the
	// nil interface gotcha.
	if c.AuthProof == nil {
		return nil
	}

	return c.AuthProof
}

// GetCapacity returns the capacity of the channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) GetCapacity() btcutil.Amount {
	return c.Capacity
}

// SetAuthProof sets the proof of the channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) SetAuthProof(proof ChannelAuthProof) error {
	if proof == nil {
		c.AuthProof = nil

		return nil
	}

	p, ok := proof.(*ChannelAuthProof1)
	if !ok {
		return fmt.Errorf("expected type ChannelAuthProof1 for "+
			"ChannelEdgeInfo1, got %T", proof)
	}

	c.AuthProof = p

	return nil
}

// GetChanPoint returns the outpoint of the funding transaction of the channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) GetChanPoint() wire.OutPoint {
	return c.ChannelPoint
}

// FundingScript returns the pk script for the funding output of the
// channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo1) FundingScript() ([]byte, error) {
	legacyFundingScript := func() ([]byte, error) {
		witnessScript, err := input.GenMultiSigScript(
			c.BitcoinKey1Bytes[:], c.BitcoinKey2Bytes[:],
		)
		if err != nil {
			return nil, err
		}
		pkScript, err := input.WitnessScriptHash(witnessScript)
		if err != nil {
			return nil, err
		}

		return pkScript, nil
	}

	if len(c.Features) == 0 {
		return legacyFundingScript()
	}

	// TODO(elle): remove this taproot funding script logic once
	//  ChannelEdgeInfo2 is being used.

	// In order to make the correct funding script, we'll need to parse the
	// chanFeatures bytes into a feature vector we can interact with.
	rawFeatures := lnwire.NewRawFeatureVector()
	err := rawFeatures.Decode(bytes.NewReader(c.Features))
	if err != nil {
		return nil, fmt.Errorf("unable to parse chan feature "+
			"bits: %w", err)
	}

	chanFeatureBits := lnwire.NewFeatureVector(
		rawFeatures, lnwire.Features,
	)
	if chanFeatureBits.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	) {

		pubKey1, err := btcec.ParsePubKey(c.BitcoinKey1Bytes[:])
		if err != nil {
			return nil, err
		}
		pubKey2, err := btcec.ParsePubKey(c.BitcoinKey2Bytes[:])
		if err != nil {
			return nil, err
		}

		fundingScript, _, err := input.GenTaprootFundingScript(
			pubKey1, pubKey2, 0,
		)
		if err != nil {
			return nil, err
		}

		return fundingScript, nil
	}

	return legacyFundingScript()
}

// A compile-time check to ensure that ChannelEdgeInfo1 implements the
// ChannelEdgeInfo interface.
var _ ChannelEdgeInfo = (*ChannelEdgeInfo1)(nil)

// ChannelEdgeInfo2 describes the information about a channel announced with
// lnwire.ChannelAnnouncement2 that we will persist.
type ChannelEdgeInfo2 struct {
	lnwire.ChannelAnnouncement2

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// AuthProof is the authentication proof for this channel.
	AuthProof *ChannelAuthProof2

	nodeKey1 *btcec.PublicKey
	nodeKey2 *btcec.PublicKey
}

// Copy returns a copy of the ChannelEdgeInfo.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) Copy() ChannelEdgeInfo {
	return &ChannelEdgeInfo2{
		ChannelAnnouncement2: lnwire.ChannelAnnouncement2{
			ChainHash:       c.ChainHash,
			Features:        c.Features,
			ShortChannelID:  c.ShortChannelID,
			Capacity:        c.Capacity,
			NodeID1:         c.NodeID1,
			NodeID2:         c.NodeID2,
			BitcoinKey1:     c.BitcoinKey1,
			BitcoinKey2:     c.BitcoinKey2,
			MerkleRootHash:  c.MerkleRootHash,
			ExtraOpaqueData: c.ExtraOpaqueData,
		},
		ChannelPoint: c.ChannelPoint,
		AuthProof:    c.AuthProof,
	}
}

// Node1Bytes returns bytes of the public key of node 1.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) Node1Bytes() [33]byte {
	return c.NodeID1
}

// Node2Bytes returns bytes of the public key of node 2.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) Node2Bytes() [33]byte {
	return c.NodeID2
}

// GetChainHash returns the hash of the genesis block of the chain that the edge
// is on.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) GetChainHash() chainhash.Hash {
	return c.ChainHash
}

// GetChanID returns the channel ID.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) GetChanID() uint64 {
	return c.ShortChannelID.ToUint64()
}

// GetAuthProof returns the ChannelAuthProof for the edge.
//
// NOTE: this is part of the models.ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) GetAuthProof() ChannelAuthProof {
	// Cant just return AuthProof cause you then run into the
	// nil interface gotcha.
	if c.AuthProof == nil {
		return nil
	}

	return c.AuthProof
}

// GetCapacity returns the capacity of the channel.
//
// NOTE: this is part of the models.ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) GetCapacity() btcutil.Amount {
	return btcutil.Amount(c.Capacity)
}

// SetAuthProof sets the proof of the channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) SetAuthProof(proof ChannelAuthProof) error {
	if proof == nil {
		c.AuthProof = nil

		return nil
	}

	p, ok := proof.(*ChannelAuthProof2)
	if !ok {
		return fmt.Errorf("expected type ChannelAuthProof2 for "+
			"ChannelEdgeInfo2, got %T", proof)
	}

	c.AuthProof = p

	return nil
}

// GetChanPoint returns the outpoint of the funding transaction of the channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) GetChanPoint() wire.OutPoint {
	return c.ChannelPoint
}

// FundingScript returns the pk script for the funding output of the
// channel.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) FundingScript() ([]byte, error) {
	var (
		pubKey1 *btcec.PublicKey
		pubKey2 *btcec.PublicKey
		err     error
	)
	c.BitcoinKey1.WhenSome(func(key [33]byte) {
		pubKey1, err = btcec.ParsePubKey(key[:])
	})
	if err != nil {
		return nil, err
	}

	c.BitcoinKey2.WhenSome(func(key [33]byte) {
		pubKey2, err = btcec.ParsePubKey(key[:])
	})
	if err != nil {
		return nil, err
	}

	// TODO(elle): account for all types.
	if pubKey1 == nil || pubKey2 == nil {
		return nil, fmt.Errorf("both bitcoin keys must be set")
	}
	if c.MerkleRootHash.IsSome() {
		return nil, fmt.Errorf("dont know how to handle merkle " +
			"root hash yet")
	}

	fundingScript, _, err := input.GenTaprootFundingScript(
		pubKey1, pubKey2, 0,
	)
	if err != nil {
		return nil, err
	}

	return fundingScript, nil
}

// NodeKey1 is the identity public key of the "first" node that was involved in
// the creation of this channel. A node is considered "first" if the
// lexicographical ordering the its serialized public key is "smaller" than
// that of the other node involved in channel creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) NodeKey1() (*btcec.PublicKey, error) {
	if c.nodeKey1 != nil {
		return c.nodeKey1, nil
	}

	key, err := btcec.ParsePubKey(c.NodeID1[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey1 = key

	return key, nil
}

// NodeKey2 is the identity public key of the "second" node that was
// involved in the creation of this channel. A node is considered
// "second" if the lexicographical ordering the its serialized public
// key is "larger" than that of the other node involved in channel
// creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
//
// NOTE: this is part of the ChannelEdgeInfo interface.
func (c *ChannelEdgeInfo2) NodeKey2() (*btcec.PublicKey, error) {
	if c.nodeKey2 != nil {
		return c.nodeKey2, nil
	}

	key, err := btcec.ParsePubKey(c.NodeID2[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey2 = key

	return key, nil
}

// A compile-time check to ensure that ChannelEdgeInfo2 implements the
// ChannelEdgeInfo interface.
var _ ChannelEdgeInfo = (*ChannelEdgeInfo2)(nil)
