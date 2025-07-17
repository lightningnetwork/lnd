package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// ChannelAnnouncement2 message is used to announce the existence of a taproot
// channel between two peers in the network.
type ChannelAnnouncement2 struct {
	// Signature is a Schnorr signature over the TLV stream of the message.
	Signature Sig

	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	ChainHash tlv.RecordT[tlv.TlvType0, chainhash.Hash]

	// Features is the feature vector that encodes the features supported
	// by the target node. This field can be used to signal the type of the
	// channel, or modifications to the fields that would normally follow
	// this vector.
	Features tlv.RecordT[tlv.TlvType2, RawFeatureVector]

	// ShortChannelID is the unique description of the funding transaction,
	// or where exactly it's located within the target blockchain.
	ShortChannelID tlv.RecordT[tlv.TlvType4, ShortChannelID]

	// Capacity is the number of satoshis of the capacity of this channel.
	// It must be less than or equal to the value of the on-chain funding
	// output.
	Capacity tlv.RecordT[tlv.TlvType6, uint64]

	// NodeID1 is the numerically-lesser public key ID of one of the channel
	// operators.
	NodeID1 tlv.RecordT[tlv.TlvType8, [33]byte]

	// NodeID2 is the numerically-greater public key ID of one of the
	// channel operators.
	NodeID2 tlv.RecordT[tlv.TlvType10, [33]byte]

	// BitcoinKey1 is the public key of the key used by Node1 in the
	// construction of the on-chain funding transaction. This is an optional
	// field and only needs to be set if the 4-of-4 MuSig construction was
	// used in the creation of the message signature.
	BitcoinKey1 tlv.OptionalRecordT[tlv.TlvType12, [33]byte]

	// BitcoinKey2 is the public key of the key used by Node2 in the
	// construction of the on-chain funding transaction. This is an optional
	// field and only needs to be set if the 4-of-4 MuSig construction was
	// used in the creation of the message signature.
	BitcoinKey2 tlv.OptionalRecordT[tlv.TlvType14, [33]byte]

	// MerkleRootHash is the hash used to create the optional tweak in the
	// funding output. If this is not set but the bitcoin keys are, then
	// the funding output is a pure 2-of-2 MuSig aggregate public key.
	MerkleRootHash tlv.OptionalRecordT[tlv.TlvType16, [32]byte]

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData ExtraOpaqueData
}

// Decode deserializes a serialized AnnounceSignatures1 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) Decode(r io.Reader, _ uint32) error {
	err := ReadElement(r, &c.Signature)
	if err != nil {
		return err
	}
	c.Signature.ForceSchnorr()

	return c.DecodeTLVRecords(r)
}

// DecodeTLVRecords decodes only the TLV section of the message.
func (c *ChannelAnnouncement2) DecodeTLVRecords(r io.Reader) error {
	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	var (
		chainHash      = tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		btcKey1        = tlv.ZeroRecordT[tlv.TlvType12, [33]byte]()
		btcKey2        = tlv.ZeroRecordT[tlv.TlvType14, [33]byte]()
		merkleRootHash = tlv.ZeroRecordT[tlv.TlvType16, [32]byte]()
	)

	knownRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &chainHash, &c.Features,
		&c.ShortChannelID, &c.Capacity, &c.NodeID1, &c.NodeID2,
		&btcKey1, &btcKey2, &merkleRootHash,
	)
	if err != nil {
		return err
	}

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash.Val = *chaincfg.MainNetParams.GenesisHash
	if _, ok := knownRecords[c.ChainHash.TlvType()]; ok {
		c.ChainHash.Val = chainHash.Val
	}

	if _, ok := knownRecords[c.BitcoinKey1.TlvType()]; ok {
		c.BitcoinKey1 = tlv.SomeRecordT(btcKey1)
	}

	if _, ok := knownRecords[c.BitcoinKey2.TlvType()]; ok {
		c.BitcoinKey2 = tlv.SomeRecordT(btcKey2)
	}

	if _, ok := knownRecords[c.MerkleRootHash.TlvType()]; ok {
		c.MerkleRootHash = tlv.SomeRecordT(merkleRootHash)
	}

	c.ExtraOpaqueData = extraData

	return c.ExtraOpaqueData.ValidateTLV()
}

// Encode serializes the target AnnounceSignatures1 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) Encode(w *bytes.Buffer, _ uint32) error {
	_, err := w.Write(c.Signature.RawBytes())
	if err != nil {
		return err
	}

	tlvRecords, err := c.DataToSign()
	if err != nil {
		return err
	}

	return WriteBytes(w, tlvRecords)
}

// DataToSign encodes the data to be signed and returns it.
func (c *ChannelAnnouncement2) DataToSign() ([]byte, error) {
	producers, err := c.ExtraOpaqueData.RecordProducers()
	if err != nil {
		return nil, err
	}

	// The chain-hash record is only included if it is _not_ equal to the
	// bitcoin mainnet genisis block hash.
	if !c.ChainHash.Val.IsEqual(chaincfg.MainNetParams.GenesisHash) {
		hash := tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		hash.Val = c.ChainHash.Val
		producers = append(producers, &hash)
	}

	producers = append(producers,
		&c.Features, &c.ShortChannelID, &c.Capacity, &c.NodeID1,
		&c.NodeID2,
	)

	c.BitcoinKey1.WhenSome(func(key tlv.RecordT[tlv.TlvType12, [33]byte]) {
		producers = append(producers, &key)
	})

	c.BitcoinKey2.WhenSome(func(key tlv.RecordT[tlv.TlvType14, [33]byte]) {
		producers = append(producers, &key)
	})

	c.MerkleRootHash.WhenSome(
		func(hash tlv.RecordT[tlv.TlvType16, [32]byte]) {
			producers = append(producers, &hash)
		},
	)

	var tlvData ExtraOpaqueData
	err = tlvData.PackRecords(producers...)
	if err != nil {
		return nil, err
	}

	return tlvData, nil
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) MsgType() MessageType {
	return MsgChannelAnnouncement2
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ChannelAnnouncement2) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.Message interface.
var _ Message = (*ChannelAnnouncement2)(nil)

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ChannelAnnouncement2)(nil)

// Node1KeyBytes returns the bytes representing the public key of node 1 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) Node1KeyBytes() [33]byte {
	return c.NodeID1.Val
}

// Node2KeyBytes returns the bytes representing the public key of node 2 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) Node2KeyBytes() [33]byte {
	return c.NodeID2.Val
}

// GetChainHash returns the hash of the chain which this channel's funding
// transaction is confirmed in.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) GetChainHash() chainhash.Hash {
	return c.ChainHash.Val
}

// SCID returns the short channel ID of the channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) SCID() ShortChannelID {
	return c.ShortChannelID.Val
}

// A compile-time check to ensure that ChannelAnnouncement2 implements the
// ChannelAnnouncement interface.
var _ ChannelAnnouncement = (*ChannelAnnouncement2)(nil)
