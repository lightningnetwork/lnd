package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ChanAnn2ChainHashType is the tlv number associated with the chain
	// hash TLV record in the channel_announcement_2 message.
	ChanAnn2ChainHashType = tlv.Type(0)

	// ChanAnn2FeaturesType is the tlv number associated with the features
	// TLV record in the channel_announcement_2 message.
	ChanAnn2FeaturesType = tlv.Type(2)

	// ChanAnn2SCIDType is the tlv number associated with the SCID TLV
	// record in the channel_announcement_2 message.
	ChanAnn2SCIDType = tlv.Type(4)

	// ChanAnn2CapacityType is the tlv number associated with the capacity
	// TLV record in the channel_announcement_2 message.
	ChanAnn2CapacityType = tlv.Type(6)

	// ChanAnn2NodeID1Type is the tlv number associated with the node ID 1
	// TLV record in the channel_announcement_2 message.
	ChanAnn2NodeID1Type = tlv.Type(8)

	// ChanAnn2NodeID2Type is the tlv number associated with the node ID 2
	// record in the channel_announcement_2 message.
	ChanAnn2NodeID2Type = tlv.Type(10)

	// ChanAnn2BtcKey1Type is the tlv number associated with the bitcoin ID
	// 1 record in the channel_announcement_2 message.
	ChanAnn2BtcKey1Type = tlv.Type(12)

	// ChanAnn2BtcKey2Type is the tlv number associated with the bitcoin ID
	// 2 record in the channel_announcement_2 message.
	ChanAnn2BtcKey2Type = tlv.Type(14)

	// ChanAnn2MerkleRootHashType is the tlv number associated with the
	// merkle root hash record in the channel_announcement_2 message.
	ChanAnn2MerkleRootHashType = tlv.Type(16)
)

// ChannelAnnouncement2 message is used to announce the existence of a taproot
// channel between two peers in the network.
type ChannelAnnouncement2 struct {
	// Signature is a Schnorr signature over the TLV stream of the message.
	Signature Sig

	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	ChainHash chainhash.Hash

	// Features is the feature vector that encodes the features supported
	// by the target node. This field can be used to signal the type of the
	// channel, or modifications to the fields that would normally follow
	// this vector.
	Features RawFeatureVector

	// ShortChannelID is the unique description of the funding transaction,
	// or where exactly it's located within the target blockchain.
	ShortChannelID ShortChannelID

	// Capacity is the number of satoshis of the capacity of this channel.
	// It must be less than or equal to the value of the on-chain funding
	// output.
	Capacity uint64

	// NodeID1 is the numerically-lesser public key ID of one of the channel
	// operators.
	NodeID1 [33]byte

	// NodeID2 is the numerically-greater public key ID of one of the
	// channel operators.
	NodeID2 [33]byte

	// BitcoinKey1 is the public key of the key used by Node1 in the
	// construction of the on-chain funding transaction. This is an optional
	// field and only needs to be set if the 4-of-4 MuSig construction was
	// used in the creation of the message signature.
	BitcoinKey1 fn.Option[[33]byte]

	// BitcoinKey2 is the public key of the key used by Node2 in the
	// construction of the on-chain funding transaction. This is an optional
	// field and only needs to be set if the 4-of-4 MuSig construction was
	// used in the creation of the message signature.
	BitcoinKey2 fn.Option[[33]byte]

	// MerkleRootHash is the hash used to create the optional tweak in the
	// funding output. If this is not set but the bitcoin keys are, then
	// the funding output is a pure 2-of-2 MuSig aggregate public key.
	MerkleRootHash fn.Option[[32]byte]

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
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	featuresRecordProducer := NewRawFeatureVectorRecordProducer(
		ChanAnn2FeaturesType,
	)

	scidRecordProducer := NewShortChannelIDRecordProducer(
		ChanAnn2SCIDType,
	)

	var (
		chainHash, merkleRootHash [32]byte
		btcKey1, btcKey2          [33]byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(ChanAnn2ChainHashType, &chainHash),
		featuresRecordProducer.Record(),
		scidRecordProducer.Record(),
		tlv.MakePrimitiveRecord(ChanAnn2CapacityType, &c.Capacity),
		tlv.MakePrimitiveRecord(ChanAnn2NodeID1Type, &c.NodeID1),
		tlv.MakePrimitiveRecord(ChanAnn2NodeID2Type, &c.NodeID2),
		tlv.MakePrimitiveRecord(ChanAnn2BtcKey1Type, &btcKey1),
		tlv.MakePrimitiveRecord(ChanAnn2BtcKey2Type, &btcKey2),
		tlv.MakePrimitiveRecord(
			ChanAnn2MerkleRootHashType, &merkleRootHash,
		),
	}

	typeMap, err := tlvRecords.ExtractRecords(records...)
	if err != nil {
		return err
	}

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash = *chaincfg.MainNetParams.GenesisHash
	if _, ok := typeMap[ChanAnn2ChainHashType]; ok {
		c.ChainHash = chainHash
	}

	if _, ok := typeMap[ChanAnn2FeaturesType]; ok {
		c.Features = featuresRecordProducer.RawFeatureVector
	}

	if _, ok := typeMap[ChanAnn2SCIDType]; ok {
		c.ShortChannelID = scidRecordProducer.ShortChannelID
	}

	if _, ok := typeMap[ChanAnn2BtcKey1Type]; ok {
		c.BitcoinKey1 = fn.Some(btcKey1)
	}

	if _, ok := typeMap[ChanAnn2BtcKey2Type]; ok {
		c.BitcoinKey2 = fn.Some(btcKey2)
	}

	if _, ok := typeMap[ChanAnn2MerkleRootHashType]; ok {
		c.MerkleRootHash = fn.Some(merkleRootHash)
	}

	if len(tlvRecords) != 0 {
		c.ExtraOpaqueData = tlvRecords
	}

	return nil
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
	_, err = c.DataToSign()
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraOpaqueData)
}

// DigestToSign computes the digest of the message to be signed.
func (c *ChannelAnnouncement2) DigestToSign() (*chainhash.Hash, error) {
	data, err := c.DataToSign()
	if err != nil {
		return nil, err
	}

	hash := MsgHash(
		"channel_announcement_2", "announcement_signature", data,
	)

	return hash, nil
}

// DataToSign encodes the data to be signed into the ExtraOpaqueData member and
// returns it.
func (c *ChannelAnnouncement2) DataToSign() ([]byte, error) {
	// The chain-hash record is only included if it is _not_ equal to the
	// bitcoin mainnet genisis block hash.
	var records []tlv.Record
	if !c.ChainHash.IsEqual(chaincfg.MainNetParams.GenesisHash) {
		chainHash := [32]byte(c.ChainHash)
		records = append(records, tlv.MakePrimitiveRecord(
			ChanAnn2ChainHashType, &chainHash,
		))
	}

	featuresRecordProducer := &RawFeatureVectorRecordProducer{
		RawFeatureVector: c.Features,
		Type:             ChanAnn2FeaturesType,
	}

	scidRecordProducer := &ShortChannelIDRecordProducer{
		ShortChannelID: c.ShortChannelID,
		Type:           ChanAnn2SCIDType,
	}

	records = append(records,
		featuresRecordProducer.Record(),
		scidRecordProducer.Record(),
		tlv.MakePrimitiveRecord(ChanAnn2CapacityType, &c.Capacity),
		tlv.MakePrimitiveRecord(ChanAnn2NodeID1Type, &c.NodeID1),
		tlv.MakePrimitiveRecord(ChanAnn2NodeID2Type, &c.NodeID2),
	)

	c.BitcoinKey1.WhenSome(func(k [33]byte) {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanAnn2BtcKey1Type, &k,
		))
	})

	c.BitcoinKey2.WhenSome(func(k [33]byte) {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanAnn2BtcKey2Type, &k,
		))
	})

	c.MerkleRootHash.WhenSome(func(hash [32]byte) {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanAnn2MerkleRootHashType, &hash,
		))
	})

	err := EncodeMessageExtraDataFromRecords(&c.ExtraOpaqueData, records...)
	if err != nil {
		return nil, err
	}

	return c.ExtraOpaqueData, nil
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement2) MsgType() MessageType {
	return MsgChannelAnnouncement2
}

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.Message interface.
var _ Message = (*ChannelAnnouncement2)(nil)

// Node1KeyBytes returns the bytes representing the public key of node 1 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) Node1KeyBytes() [33]byte {
	return c.NodeID1
}

// Node2KeyBytes returns the bytes representing the public key of node 2 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) Node2KeyBytes() [33]byte {
	return c.NodeID2
}

// GetChainHash returns the hash of the chain which this channel's funding
// transaction is confirmed in.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) GetChainHash() chainhash.Hash {
	return c.ChainHash
}

// SCID returns the short channel ID of the channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (c *ChannelAnnouncement2) SCID() ShortChannelID {
	return c.ShortChannelID
}

// A compile-time check to ensure that ChannelAnnouncement2 implements the
// ChannelAnnouncement interface.
var _ ChannelAnnouncement = (*ChannelAnnouncement2)(nil)
