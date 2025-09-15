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

	// Outpoint is the outpoint of the funding transaction.
	Outpoint tlv.RecordT[tlv.TlvType18, OutPoint]

	// Signature is a Schnorr signature over serialised signed-range TLV
	// stream of the message.
	Signature tlv.RecordT[tlv.TlvType160, Sig]

	// Any extra fields in the signed range that we do not yet know about,
	// but we need to keep them for signature validation and to produce a
	// valid message.
	ExtraSignedFields
}

// Encode serializes the target AnnounceSignatures1 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) Encode(w *bytes.Buffer, _ uint32) error {
	return EncodePureTLVMessage(c, w)
}

// AllRecords returns all the TLV records for the message. This will include all
// the records we know about along with any that we don't know about but that
// fall in the signed TLV range.
//
// NOTE: this is part of the PureTLVMessage interface.
func (c *ChannelAnnouncement2) AllRecords() []tlv.Record {
	recordProducers := append(
		c.nonSignatureRecordProducers(), &c.Signature,
	)

	return ProduceRecordsSorted(recordProducers...)
}

// nonSignatureRecordProducers returns all the TLV record producers for the
// message except the signature record producer.
//
//nolint:ll
func (c *ChannelAnnouncement2) nonSignatureRecordProducers() []tlv.RecordProducer {
	// The chain-hash record is only included if it is _not_ equal to the
	// bitcoin mainnet genisis block hash.
	var recordProducers []tlv.RecordProducer
	if !c.ChainHash.Val.IsEqual(chaincfg.MainNetParams.GenesisHash) {
		hash := tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		hash.Val = c.ChainHash.Val

		recordProducers = append(recordProducers, &hash)
	}

	recordProducers = append(recordProducers,
		&c.Features, &c.ShortChannelID, &c.Capacity, &c.NodeID1,
		&c.NodeID2,
	)

	c.BitcoinKey1.WhenSome(func(key tlv.RecordT[tlv.TlvType12, [33]byte]) {
		recordProducers = append(recordProducers, &key)
	})

	c.BitcoinKey2.WhenSome(func(key tlv.RecordT[tlv.TlvType14, [33]byte]) {
		recordProducers = append(recordProducers, &key)
	})

	c.MerkleRootHash.WhenSome(
		func(hash tlv.RecordT[tlv.TlvType16, [32]byte]) {
			recordProducers = append(recordProducers, &hash)
		},
	)

	recordProducers = append(recordProducers, &c.Outpoint)
	recordProducers = append(recordProducers, RecordsAsProducers(
		tlv.MapToRecords(c.ExtraSignedFields),
	)...)

	return recordProducers
}

// Decode deserializes a serialized AnnounceSignatures1 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) Decode(r io.Reader, _ uint32) error {
	var (
		chainHash      = tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		btcKey1        = tlv.ZeroRecordT[tlv.TlvType12, [33]byte]()
		btcKey2        = tlv.ZeroRecordT[tlv.TlvType14, [33]byte]()
		merkleRootHash = tlv.ZeroRecordT[tlv.TlvType16, [32]byte]()
	)
	stream, err := tlv.NewStream(ProduceRecordsSorted(
		&chainHash,
		&c.Features,
		&c.ShortChannelID,
		&c.Capacity,
		&c.NodeID1,
		&c.NodeID2,
		&btcKey1,
		&btcKey2,
		&merkleRootHash,
		&c.Outpoint,
		&c.Signature,
	)...)
	if err != nil {
		return err
	}
	c.Signature.Val.ForceSchnorr()

	typeMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return err
	}

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash.Val = *chaincfg.MainNetParams.GenesisHash
	if _, ok := typeMap[c.ChainHash.TlvType()]; ok {
		c.ChainHash.Val = chainHash.Val
	}

	if _, ok := typeMap[c.BitcoinKey1.TlvType()]; ok {
		c.BitcoinKey1 = tlv.SomeRecordT(btcKey1)
	}

	if _, ok := typeMap[c.BitcoinKey2.TlvType()]; ok {
		c.BitcoinKey2 = tlv.SomeRecordT(btcKey2)
	}

	if _, ok := typeMap[c.MerkleRootHash.TlvType()]; ok {
		c.MerkleRootHash = tlv.SomeRecordT(merkleRootHash)
	}

	c.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// DecodeNonSigTLVRecords decodes only the TLV section of the message.
func (c *ChannelAnnouncement2) DecodeNonSigTLVRecords(r io.Reader) error {
	var (
		chainHash      = tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		btcKey1        = tlv.ZeroRecordT[tlv.TlvType12, [33]byte]()
		btcKey2        = tlv.ZeroRecordT[tlv.TlvType14, [33]byte]()
		merkleRootHash = tlv.ZeroRecordT[tlv.TlvType16, [32]byte]()
	)
	stream, err := tlv.NewStream(ProduceRecordsSorted(
		&chainHash,
		&c.Features,
		&c.ShortChannelID,
		&c.Capacity,
		&c.NodeID1,
		&c.NodeID2,
		&btcKey1,
		&btcKey2,
		&merkleRootHash,
		&c.Outpoint,
	)...)
	if err != nil {
		return err
	}

	typeMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return err
	}

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash.Val = *chaincfg.MainNetParams.GenesisHash
	if _, ok := typeMap[c.ChainHash.TlvType()]; ok {
		c.ChainHash.Val = chainHash.Val
	}

	if _, ok := typeMap[c.BitcoinKey1.TlvType()]; ok {
		c.BitcoinKey1 = tlv.SomeRecordT(btcKey1)
	}

	if _, ok := typeMap[c.BitcoinKey2.TlvType()]; ok {
		c.BitcoinKey2 = tlv.SomeRecordT(btcKey2)
	}

	if _, ok := typeMap[c.MerkleRootHash.TlvType()]; ok {
		c.MerkleRootHash = tlv.SomeRecordT(merkleRootHash)
	}

	c.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// EncodeAllNonSigFields encodes the entire message to the given writer but
// excludes the signature field.
func (c *ChannelAnnouncement2) EncodeAllNonSigFields(w io.Writer) error {
	return EncodeRecordsTo(
		w, ProduceRecordsSorted(c.nonSignatureRecordProducers()...),
	)
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

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.PureTLVMessage interface.
var _ PureTLVMessage = (*ChannelAnnouncement2)(nil)

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

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (c *ChannelAnnouncement2) GossipVersion() GossipVersion {
	return GossipVersion2
}

// A compile-time check to ensure that ChannelAnnouncement2 implements the
// ChannelAnnouncement interface.
var _ ChannelAnnouncement = (*ChannelAnnouncement2)(nil)
