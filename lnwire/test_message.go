package lnwire

import (
	"bytes"
	"fmt"
	"image/color"
	"math"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestMessage is an interface that extends the base Message interface with a
// method to populate the message with random testing data.
type TestMessage interface {
	Message

	// RandTestMessage populates the message with random data suitable for
	// testing. It uses the rapid testing framework to generate random
	// values.
	RandTestMessage(t *rapid.T) Message
}

// A compile time check to ensure AcceptChannel implements the TestMessage
// interface.
var _ TestMessage = (*AcceptChannel)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *AcceptChannel) RandTestMessage(t *rapid.T) Message {
	var pendingChanID [32]byte
	pendingChanIDBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
		t, "pendingChanID",
	)
	copy(pendingChanID[:], pendingChanIDBytes)

	var channelType *ChannelType
	includeChannelType := rapid.Bool().Draw(t, "includeChannelType")
	includeLeaseExpiry := rapid.Bool().Draw(t, "includeLeaseExpiry")
	includeLocalNonce := rapid.Bool().Draw(t, "includeLocalNonce")

	if includeChannelType {
		channelType = RandChannelType(t)
	}

	var leaseExpiry *LeaseExpiry
	if includeLeaseExpiry {
		leaseExpiry = RandLeaseExpiry(t)
	}

	var localNonce OptMusig2NonceTLV
	if includeLocalNonce {
		nonce := RandMusig2Nonce(t)
		localNonce = tlv.SomeRecordT(
			tlv.NewRecordT[NonceRecordTypeT, Musig2Nonce](nonce),
		)
	}

	return &AcceptChannel{
		PendingChannelID: pendingChanID,
		DustLimit: btcutil.Amount(
			rapid.IntRange(100, 1000).Draw(t, "dustLimit"),
		),
		MaxValueInFlight: MilliSatoshi(
			rapid.IntRange(10000, 1000000).Draw(
				t, "maxValueInFlight",
			),
		),
		ChannelReserve: btcutil.Amount(
			rapid.IntRange(1000, 10000).Draw(t, "channelReserve"),
		),
		HtlcMinimum: MilliSatoshi(
			rapid.IntRange(1, 1000).Draw(t, "htlcMinimum"),
		),
		MinAcceptDepth: uint32(
			rapid.IntRange(1, 10).Draw(t, "minAcceptDepth"),
		),
		CsvDelay: uint16(
			rapid.IntRange(144, 1000).Draw(t, "csvDelay"),
		),
		MaxAcceptedHTLCs: uint16(
			rapid.IntRange(10, 500).Draw(t, "maxAcceptedHTLCs"),
		),
		FundingKey:            RandPubKey(t),
		RevocationPoint:       RandPubKey(t),
		PaymentPoint:          RandPubKey(t),
		DelayedPaymentPoint:   RandPubKey(t),
		HtlcPoint:             RandPubKey(t),
		FirstCommitmentPoint:  RandPubKey(t),
		UpfrontShutdownScript: RandDeliveryAddress(t),
		ChannelType:           channelType,
		LeaseExpiry:           leaseExpiry,
		LocalNonce:            localNonce,
		ExtraData:             RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure AnnounceSignatures1 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*AnnounceSignatures1)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *AnnounceSignatures1) RandTestMessage(t *rapid.T) Message {
	return &AnnounceSignatures1{
		ChannelID:        RandChannelID(t),
		ShortChannelID:   RandShortChannelID(t),
		NodeSignature:    RandSignature(t),
		BitcoinSignature: RandSignature(t),
		ExtraOpaqueData:  RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure AnnounceSignatures2 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*AnnounceSignatures2)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *AnnounceSignatures2) RandTestMessage(t *rapid.T) Message {
	var (
		chanID = RandChannelID(t)
		scid   = RandShortChannelID(t)
		pSig   = RandPartialSig(t)
	)

	msg := &AnnounceSignatures2{
		ChannelID: tlv.NewRecordT[tlv.TlvType0, ChannelID](
			chanID,
		),
		ShortChannelID: tlv.NewRecordT[tlv.TlvType2](scid),
		PartialSignature: tlv.NewRecordT[tlv.TlvType4, PartialSig](
			*pSig,
		),
		ExtraSignedFields: make(map[uint64][]byte),
	}

	randRecs, _ := RandSignedRangeRecords(t)
	if len(randRecs) > 0 {
		msg.ExtraSignedFields = ExtraSignedFields(randRecs)
	}

	return msg
}

// A compile time check to ensure ChannelAnnouncement1 implements the
// TestMessage interface.
var _ TestMessage = (*ChannelAnnouncement1)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *ChannelAnnouncement1) RandTestMessage(t *rapid.T) Message {
	// Generate Node IDs and Bitcoin keys (compressed public keys)
	node1PubKey := RandPubKey(t)
	node2PubKey := RandPubKey(t)
	bitcoin1PubKey := RandPubKey(t)
	bitcoin2PubKey := RandPubKey(t)

	// Convert to byte arrays
	var nodeID1, nodeID2, bitcoinKey1, bitcoinKey2 [33]byte
	copy(nodeID1[:], node1PubKey.SerializeCompressed())
	copy(nodeID2[:], node2PubKey.SerializeCompressed())
	copy(bitcoinKey1[:], bitcoin1PubKey.SerializeCompressed())
	copy(bitcoinKey2[:], bitcoin2PubKey.SerializeCompressed())

	// Ensure nodeID1 is numerically less than nodeID2
	// This is a requirement stated in the field description
	if bytes.Compare(nodeID1[:], nodeID2[:]) > 0 {
		nodeID1, nodeID2 = nodeID2, nodeID1
	}

	// Generate chain hash
	chainHash := RandChainHash(t)
	var hash chainhash.Hash
	copy(hash[:], chainHash[:])

	return &ChannelAnnouncement1{
		NodeSig1:        RandSignature(t),
		NodeSig2:        RandSignature(t),
		BitcoinSig1:     RandSignature(t),
		BitcoinSig2:     RandSignature(t),
		Features:        RandFeatureVector(t),
		ChainHash:       hash,
		ShortChannelID:  RandShortChannelID(t),
		NodeID1:         nodeID1,
		NodeID2:         nodeID2,
		BitcoinKey1:     bitcoinKey1,
		BitcoinKey2:     bitcoinKey2,
		ExtraOpaqueData: RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ChannelAnnouncement2)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ChannelAnnouncement2) RandTestMessage(t *rapid.T) Message {
	features := RandFeatureVector(t)
	shortChanID := RandShortChannelID(t)
	capacity := uint64(rapid.IntRange(1, 16777215).Draw(t, "capacity"))

	var nodeID1, nodeID2 [33]byte
	copy(nodeID1[:], RandPubKey(t).SerializeCompressed())
	copy(nodeID2[:], RandPubKey(t).SerializeCompressed())

	// Make sure nodeID1 is numerically less than nodeID2 (as per spec).
	if bytes.Compare(nodeID1[:], nodeID2[:]) > 0 {
		nodeID1, nodeID2 = nodeID2, nodeID1
	}

	chainHash := RandChainHash(t)
	var chainHashObj chainhash.Hash
	copy(chainHashObj[:], chainHash[:])

	msg := &ChannelAnnouncement2{
		ChainHash: tlv.NewPrimitiveRecord[tlv.TlvType0, chainhash.Hash](
			chainHashObj,
		),
		Features: tlv.NewRecordT[tlv.TlvType2, RawFeatureVector](
			*features,
		),
		ShortChannelID: tlv.NewRecordT[tlv.TlvType4, ShortChannelID](
			shortChanID,
		),
		Capacity: tlv.NewPrimitiveRecord[tlv.TlvType6, uint64](
			capacity,
		),
		NodeID1: tlv.NewPrimitiveRecord[tlv.TlvType8, [33]byte](
			nodeID1,
		),
		NodeID2: tlv.NewPrimitiveRecord[tlv.TlvType10, [33]byte](
			nodeID2,
		),
		ExtraSignedFields: make(map[uint64][]byte),
	}

	msg.Signature.Val = RandSignature(t)
	msg.Signature.Val.ForceSchnorr()

	randRecs, _ := RandSignedRangeRecords(t)
	if len(randRecs) > 0 {
		msg.ExtraSignedFields = ExtraSignedFields(randRecs)
	}

	// Randomly include optional fields
	if rapid.Bool().Draw(t, "includeBitcoinKey1") {
		var bitcoinKey1 [33]byte
		copy(bitcoinKey1[:], RandPubKey(t).SerializeCompressed())
		msg.BitcoinKey1 = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType12, [33]byte](
				bitcoinKey1,
			),
		)
	}

	if rapid.Bool().Draw(t, "includeBitcoinKey2") {
		var bitcoinKey2 [33]byte
		copy(bitcoinKey2[:], RandPubKey(t).SerializeCompressed())
		msg.BitcoinKey2 = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType14, [33]byte](
				bitcoinKey2,
			),
		)
	}

	if rapid.Bool().Draw(t, "includeMerkleRootHash") {
		hash := RandSHA256Hash(t)
		var merkleRootHash [32]byte
		copy(merkleRootHash[:], hash[:])
		msg.MerkleRootHash = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType16, [32]byte](
				merkleRootHash,
			),
		)
	}

	msg.Outpoint = tlv.NewRecordT[tlv.TlvType18, OutPoint](
		OutPoint(RandOutPoint(t)),
	)

	return msg
}

// A compile time check to ensure ChannelReady implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*ChannelReady)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ChannelReady) RandTestMessage(t *rapid.T) Message {
	msg := &ChannelReady{
		ChanID:                 RandChannelID(t),
		NextPerCommitmentPoint: RandPubKey(t),
		ExtraData:              RandExtraOpaqueData(t, nil),
	}

	includeAliasScid := rapid.Bool().Draw(t, "includeAliasScid")
	includeNextLocalNonce := rapid.Bool().Draw(t, "includeNextLocalNonce")
	includeAnnouncementNodeNonce := rapid.Bool().Draw(
		t, "includeAnnouncementNodeNonce",
	)
	includeAnnouncementBitcoinNonce := rapid.Bool().Draw(
		t, "includeAnnouncementBitcoinNonce",
	)

	if includeAliasScid {
		scid := RandShortChannelID(t)
		msg.AliasScid = &scid
	}

	if includeNextLocalNonce {
		nonce := RandMusig2Nonce(t)
		msg.NextLocalNonce = SomeMusig2Nonce(nonce)
	}

	if includeAnnouncementNodeNonce {
		nonce := RandMusig2Nonce(t)
		msg.AnnouncementNodeNonce = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType0, Musig2Nonce](nonce),
		)
	}

	if includeAnnouncementBitcoinNonce {
		nonce := RandMusig2Nonce(t)
		msg.AnnouncementBitcoinNonce = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2, Musig2Nonce](nonce),
		)
	}

	return msg
}

// A compile time check to ensure ChannelReestablish implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ChannelReestablish)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *ChannelReestablish) RandTestMessage(t *rapid.T) Message {
	msg := &ChannelReestablish{
		ChanID: RandChannelID(t),
		NextLocalCommitHeight: rapid.Uint64().Draw(
			t, "nextLocalCommitHeight",
		),
		RemoteCommitTailHeight: rapid.Uint64().Draw(
			t, "remoteCommitTailHeight",
		),
		LastRemoteCommitSecret:    RandPaymentPreimage(t),
		LocalUnrevokedCommitPoint: RandPubKey(t),
		ExtraData:                 RandExtraOpaqueData(t, nil),
	}

	// Randomly decide whether to include optional fields
	includeLocalNonce := rapid.Bool().Draw(t, "includeLocalNonce")
	includeDynHeight := rapid.Bool().Draw(t, "includeDynHeight")

	if includeLocalNonce {
		nonce := RandMusig2Nonce(t)
		msg.LocalNonce = SomeMusig2Nonce(nonce)
	}

	if includeDynHeight {
		height := DynHeight(rapid.Uint64().Draw(t, "dynHeight"))
		msg.DynHeight = fn.Some(height)
	}

	return msg
}

// A compile time check to ensure ChannelUpdate1 implements the TestMessage
// interface.
var _ TestMessage = (*ChannelUpdate1)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *ChannelUpdate1) RandTestMessage(t *rapid.T) Message {
	// Generate random message flags
	// Randomly decide whether to include max HTLC field
	includeMaxHtlc := rapid.Bool().Draw(t, "includeMaxHtlc")
	var msgFlags ChanUpdateMsgFlags
	if includeMaxHtlc {
		msgFlags |= ChanUpdateRequiredMaxHtlc
	}

	// Generate random channel flags
	// Randomly decide direction (node1 or node2)
	isNode2 := rapid.Bool().Draw(t, "isNode2")
	var chanFlags ChanUpdateChanFlags
	if isNode2 {
		chanFlags |= ChanUpdateDirection
	}

	// Randomly decide if channel is disabled
	isDisabled := rapid.Bool().Draw(t, "isDisabled")
	if isDisabled {
		chanFlags |= ChanUpdateDisabled
	}

	// Generate chain hash
	chainHash := RandChainHash(t)
	var hash chainhash.Hash
	copy(hash[:], chainHash[:])

	// Generate other random fields
	maxHtlc := MilliSatoshi(rapid.Uint64().Draw(t, "maxHtlc"))

	// If max HTLC flag is not set, we need to zero the value
	if !includeMaxHtlc {
		maxHtlc = 0
	}

	// Randomly decide if an inbound fee should be included.
	// By default, our extra opaque data will just be random TLV but if we
	// include an inbound fee, then we will also set the record in the
	// extra opaque data.
	var (
		customRecords, _ = RandCustomRecords(t, nil)
		inboundFee       tlv.OptionalRecordT[tlv.TlvType55555, Fee]
	)
	includeInboundFee := rapid.Bool().Draw(t, "includeInboundFee")
	if includeInboundFee {
		if customRecords == nil {
			customRecords = make(CustomRecords)
		}

		inFeeBase := int32(
			rapid.IntRange(-1000, 1000).Draw(t, "inFeeBase"),
		)
		inFeeProp := int32(
			rapid.IntRange(-1000, 1000).Draw(t, "inFeeProp"),
		)
		fee := Fee{
			BaseFee: inFeeBase,
			FeeRate: inFeeProp,
		}
		inboundFee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType55555, Fee](fee),
		)

		var b bytes.Buffer
		feeRecord := fee.Record()
		err := feeRecord.Encode(&b)
		require.NoError(t, err)

		customRecords[uint64(FeeRecordType)] = b.Bytes()
	}

	extraBytes, err := customRecords.Serialize()
	require.NoError(t, err)

	return &ChannelUpdate1{
		Signature:      RandSignature(t),
		ChainHash:      hash,
		ShortChannelID: RandShortChannelID(t),
		Timestamp: uint32(rapid.IntRange(0, 0x7FFFFFFF).Draw(
			t, "timestamp"),
		),
		MessageFlags: msgFlags,
		ChannelFlags: chanFlags,
		TimeLockDelta: uint16(rapid.IntRange(0, 65535).Draw(
			t, "timelockDelta"),
		),
		HtlcMinimumMsat: MilliSatoshi(rapid.Uint64().Draw(
			t, "htlcMinimum"),
		),
		BaseFee: uint32(rapid.IntRange(0, 0x7FFFFFFF).Draw(
			t, "baseFee"),
		),
		FeeRate: uint32(rapid.IntRange(0, 0x7FFFFFFF).Draw(
			t, "feeRate"),
		),
		HtlcMaximumMsat: maxHtlc,
		InboundFee:      inboundFee,
		ExtraOpaqueData: extraBytes,
	}
}

// A compile time check to ensure ChannelUpdate2 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ChannelUpdate2)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ChannelUpdate2) RandTestMessage(t *rapid.T) Message {
	shortChanID := RandShortChannelID(t)
	blockHeight := uint32(rapid.IntRange(0, 1000000).Draw(t, "blockHeight"))

	var disabledFlags ChanUpdateDisableFlags
	if rapid.Bool().Draw(t, "disableIncoming") {
		disabledFlags |= ChanUpdateDisableIncoming
	}
	if rapid.Bool().Draw(t, "disableOutgoing") {
		disabledFlags |= ChanUpdateDisableOutgoing
	}

	cltvExpiryDelta := uint16(rapid.IntRange(10, 200).Draw(
		t, "cltvExpiryDelta"),
	)

	htlcMinMsat := MilliSatoshi(rapid.IntRange(1, 10000).Draw(
		t, "htlcMinMsat"),
	)
	htlcMaxMsat := MilliSatoshi(rapid.IntRange(10000, 100000000).Draw(
		t, "htlcMaxMsat"),
	)
	feeBaseMsat := uint32(rapid.IntRange(0, 10000).Draw(t, "feeBaseMsat"))
	feeProportionalMillionths := uint32(rapid.IntRange(0, 10000).Draw(
		t, "feeProportionalMillionths"),
	)

	chainHash := RandChainHash(t)
	var chainHashObj chainhash.Hash
	copy(chainHashObj[:], chainHash[:])

	//nolint:ll
	msg := &ChannelUpdate2{
		ChainHash: tlv.NewPrimitiveRecord[tlv.TlvType0, chainhash.Hash](
			chainHashObj,
		),
		ShortChannelID: tlv.NewRecordT[tlv.TlvType2, ShortChannelID](
			shortChanID,
		),
		BlockHeight: tlv.NewPrimitiveRecord[tlv.TlvType4, uint32](
			blockHeight,
		),
		DisabledFlags: tlv.NewPrimitiveRecord[tlv.TlvType6, ChanUpdateDisableFlags]( //nolint:ll
			disabledFlags,
		),
		CLTVExpiryDelta: tlv.NewPrimitiveRecord[tlv.TlvType10, uint16](
			cltvExpiryDelta,
		),
		HTLCMinimumMsat: tlv.NewPrimitiveRecord[tlv.TlvType12, MilliSatoshi](
			htlcMinMsat,
		),
		HTLCMaximumMsat: tlv.NewPrimitiveRecord[tlv.TlvType14, MilliSatoshi](
			htlcMaxMsat,
		),
		FeeBaseMsat: tlv.NewPrimitiveRecord[tlv.TlvType16, uint32](
			feeBaseMsat,
		),
		FeeProportionalMillionths: tlv.NewPrimitiveRecord[tlv.TlvType18, uint32](
			feeProportionalMillionths,
		),
		ExtraSignedFields: make(map[uint64][]byte),
	}

	if rapid.Bool().Draw(t, "includeInboundFee") {
		base := rapid.IntRange(-1000, 1000).Draw(t, "inFeeBase")
		rate := rapid.IntRange(-1000, 1000).Draw(t, "inFeeProp")
		fee := Fee{
			BaseFee: int32(base),
			FeeRate: int32(rate),
		}
		msg.InboundFee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType55555](fee),
		)
	}

	msg.Signature.Val = RandSignature(t)
	msg.Signature.Val.ForceSchnorr()

	if rapid.Bool().Draw(t, "isSecondPeer") {
		msg.SecondPeer = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType8, TrueBoolean]{},
		)
	}

	return msg
}

// A compile time check to ensure ClosingComplete implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ClosingComplete)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ClosingComplete) RandTestMessage(t *rapid.T) Message {
	msg := &ClosingComplete{
		ChannelID: RandChannelID(t),
		FeeSatoshis: btcutil.Amount(rapid.Int64Range(0, 1000000).Draw(
			t, "feeSatoshis"),
		),
		LockTime: rapid.Uint32Range(0, 0xffffffff).Draw(
			t, "lockTime",
		),
		CloseeScript: RandDeliveryAddress(t),
		CloserScript: RandDeliveryAddress(t),
		ExtraData:    RandExtraOpaqueData(t, nil),
	}

	includeCloserNoClosee := rapid.Bool().Draw(t, "includeCloserNoClosee")
	includeNoCloserClosee := rapid.Bool().Draw(t, "includeNoCloserClosee")
	includeCloserAndClosee := rapid.Bool().Draw(t, "includeCloserAndClosee")

	// Ensure at least one signature is present.
	if !includeCloserNoClosee && !includeNoCloserClosee &&
		!includeCloserAndClosee {

		// If all are false, enable at least one randomly.
		choice := rapid.IntRange(0, 2).Draw(t, "sigChoice")
		switch choice {
		case 0:
			includeCloserNoClosee = true
		case 1:
			includeNoCloserClosee = true
		case 2:
			includeCloserAndClosee = true
		}
	}

	if includeCloserNoClosee {
		sig := RandSignature(t)
		msg.CloserNoClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType1, Sig](sig),
		)
	}

	if includeNoCloserClosee {
		sig := RandSignature(t)
		msg.NoCloserClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2, Sig](sig),
		)
	}

	if includeCloserAndClosee {
		sig := RandSignature(t)
		msg.CloserAndClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3, Sig](sig),
		)
	}

	return msg
}

// A compile time check to ensure ClosingSig implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*ClosingSig)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ClosingSig) RandTestMessage(t *rapid.T) Message {
	msg := &ClosingSig{
		ChannelID:    RandChannelID(t),
		CloseeScript: RandDeliveryAddress(t),
		CloserScript: RandDeliveryAddress(t),
		ExtraData:    RandExtraOpaqueData(t, nil),
	}

	includeCloserNoClosee := rapid.Bool().Draw(t, "includeCloserNoClosee")
	includeNoCloserClosee := rapid.Bool().Draw(t, "includeNoCloserClosee")
	includeCloserAndClosee := rapid.Bool().Draw(t, "includeCloserAndClosee")

	// Ensure at least one signature is present.
	if !includeCloserNoClosee && !includeNoCloserClosee &&
		!includeCloserAndClosee {

		// If all are false, enable at least one randomly.
		choice := rapid.IntRange(0, 2).Draw(t, "sigChoice")
		switch choice {
		case 0:
			includeCloserNoClosee = true
		case 1:
			includeNoCloserClosee = true
		case 2:
			includeCloserAndClosee = true
		}
	}

	if includeCloserNoClosee {
		sig := RandSignature(t)
		msg.CloserNoClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType1, Sig](sig),
		)
	}

	if includeNoCloserClosee {
		sig := RandSignature(t)
		msg.NoCloserClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2, Sig](sig),
		)
	}

	if includeCloserAndClosee {
		sig := RandSignature(t)
		msg.CloserAndClosee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3, Sig](sig),
		)
	}

	return msg
}

// A compile time check to ensure ClosingSigned implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ClosingSigned)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ClosingSigned) RandTestMessage(t *rapid.T) Message {
	// Generate a random boolean to decide whether to include CommitSig or
	// PartialSig Since they're mutually exclusive, when one is populated,
	// the other must be blank.
	usePartialSig := rapid.Bool().Draw(t, "usePartialSig")

	msg := &ClosingSigned{
		ChannelID: RandChannelID(t),
		FeeSatoshis: btcutil.Amount(
			rapid.Int64Range(0, 1000000).Draw(t, "feeSatoshis"),
		),
		ExtraData: RandExtraOpaqueData(t, nil),
	}

	if usePartialSig {
		sigBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			t, "sigScalar",
		)
		var s btcec.ModNScalar
		_ = s.SetByteSlice(sigBytes)

		msg.PartialSig = SomePartialSig(NewPartialSig(s))
		msg.Signature = Sig{}
	} else {
		msg.Signature = RandSignature(t)
	}

	return msg
}

// A compile time check to ensure CommitSig implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*CommitSig)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *CommitSig) RandTestMessage(t *rapid.T) Message {
	cr, _ := RandCustomRecords(t, nil)
	sig := &CommitSig{
		ChanID:        RandChannelID(t),
		CommitSig:     RandSignature(t),
		CustomRecords: cr,
	}

	numHtlcSigs := rapid.IntRange(0, 20).Draw(t, "numHtlcSigs")
	htlcSigs := make([]Sig, numHtlcSigs)
	for i := 0; i < numHtlcSigs; i++ {
		htlcSigs[i] = RandSignature(t)
	}

	if len(htlcSigs) > 0 {
		sig.HtlcSigs = htlcSigs
	}

	includePartialSig := rapid.Bool().Draw(t, "includePartialSig")
	if includePartialSig {
		sigWithNonce := RandPartialSigWithNonce(t)
		sig.PartialSig = MaybePartialSigWithNonce(sigWithNonce)
	}

	return sig
}

// A compile time check to ensure Custom implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Custom)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *Custom) RandTestMessage(t *rapid.T) Message {
	msgType := MessageType(
		rapid.IntRange(int(CustomTypeStart), 65535).Draw(
			t, "customMsgType",
		),
	)

	dataLen := rapid.IntRange(0, 1000).Draw(t, "customDataLength")
	data := rapid.SliceOfN(rapid.Byte(), dataLen, dataLen).Draw(
		t, "customData",
	)

	msg, err := NewCustom(msgType, data)
	if err != nil {
		panic(fmt.Sprintf("Error creating custom message: %v", err))
	}

	return msg
}

// A compile time check to ensure OnionMessage implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*OnionMessage)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (o *OnionMessage) RandTestMessage(t *rapid.T) Message {
	// Generate random compressed public key for node ID
	pathKey := RandPubKey(t)

	dataLen := rapid.IntRange(0, 1000).Draw(t, "onionMessageDataLength")
	data := rapid.SliceOfN(rapid.Byte(), dataLen, dataLen).Draw(
		t, "onionMessageData",
	)

	msg := NewOnionMessage(pathKey, data)

	return msg
}

// A compile time check to ensure DynAck implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*DynAck)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (da *DynAck) RandTestMessage(t *rapid.T) Message {
	msg := &DynAck{
		ChanID: RandChannelID(t),
	}

	includeLocalNonce := rapid.Bool().Draw(t, "includeLocalNonce")
	if includeLocalNonce {
		nonce := RandMusig2Nonce(t)
		rec := tlv.NewRecordT[tlv.TlvType14](nonce)
		msg.LocalNonce = tlv.SomeRecordT(rec)
	}

	// Create a tlv type lists to hold all known records which will be
	// ignored when creating ExtraData records.
	ignoreRecords := fn.NewSet[uint64]()
	for i := range uint64(15) {
		// Ignore known records.
		if i%2 == 0 {
			ignoreRecords.Add(i)
		}
	}

	msg.ExtraData = RandExtraOpaqueData(t, ignoreRecords)

	return msg
}

// A compile time check to ensure DynPropose implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*DynPropose)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (dp *DynPropose) RandTestMessage(t *rapid.T) Message {
	msg := &DynPropose{
		ChanID: RandChannelID(t),
	}

	// Randomly decide which optional fields to include
	includeDustLimit := rapid.Bool().Draw(t, "includeDustLimit")
	includeMaxValueInFlight := rapid.Bool().Draw(
		t, "includeMaxValueInFlight",
	)
	includeChannelReserve := rapid.Bool().Draw(t, "includeChannelReserve")
	includeCsvDelay := rapid.Bool().Draw(t, "includeCsvDelay")
	includeMaxAcceptedHTLCs := rapid.Bool().Draw(
		t, "includeMaxAcceptedHTLCs",
	)
	includeChannelType := rapid.Bool().Draw(t, "includeChannelType")

	// Generate random values for each included field
	if includeDustLimit {
		var rec tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
		val := btcutil.Amount(rapid.Uint32().Draw(t, "dustLimit"))
		rec.Val = tlv.NewBigSizeT(val)
		msg.DustLimit = tlv.SomeRecordT(rec)
	}

	if includeMaxValueInFlight {
		var rec tlv.RecordT[tlv.TlvType2, MilliSatoshi]
		val := MilliSatoshi(rapid.Uint64().Draw(t, "maxValueInFlight"))
		rec.Val = val
		msg.MaxValueInFlight = tlv.SomeRecordT(rec)
	}

	if includeChannelReserve {
		var rec tlv.RecordT[tlv.TlvType6, tlv.BigSizeT[btcutil.Amount]]
		val := btcutil.Amount(rapid.Uint32().Draw(t, "channelReserve"))
		rec.Val = tlv.NewBigSizeT(val)
		msg.ChannelReserve = tlv.SomeRecordT(rec)
	}

	if includeCsvDelay {
		csvDelay := msg.CsvDelay.Zero()
		val := rapid.Uint16().Draw(t, "csvDelay")
		csvDelay.Val = val
		msg.CsvDelay = tlv.SomeRecordT(csvDelay)
	}

	if includeMaxAcceptedHTLCs {
		maxHtlcs := msg.MaxAcceptedHTLCs.Zero()
		maxHtlcs.Val = rapid.Uint16().Draw(t, "maxAcceptedHTLCs")
		msg.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}

	if includeChannelType {
		chanType := msg.ChannelType.Zero()
		chanType.Val = *RandChannelType(t)
		msg.ChannelType = tlv.SomeRecordT(chanType)
	}

	// Create a tlv type lists to hold all known records which will be
	// ignored when creating ExtraData records.
	ignoreRecords := fn.NewSet[uint64]()
	for i := range uint64(13) {
		// Ignore known records.
		if i%2 == 0 {
			ignoreRecords.Add(i)
		}
	}

	msg.ExtraData = RandExtraOpaqueData(t, ignoreRecords)

	return msg
}

// A compile time check to ensure DynReject implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*DynReject)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (dr *DynReject) RandTestMessage(t *rapid.T) Message {
	featureVec := NewRawFeatureVector()

	numFeatures := rapid.IntRange(0, 8).Draw(t, "numRejections")
	for i := 0; i < numFeatures; i++ {
		bit := FeatureBit(
			rapid.IntRange(0, 31).Draw(
				t, fmt.Sprintf("rejectionBit-%d", i),
			),
		)
		featureVec.Set(bit)
	}

	var extraData ExtraOpaqueData
	randData := RandExtraOpaqueData(t, nil)
	if len(randData) > 0 {
		extraData = randData
	}

	return &DynReject{
		ChanID:           RandChannelID(t),
		UpdateRejections: *featureVec,
		ExtraData:        extraData,
	}
}

// A compile time check to ensure DynCommit implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*DynCommit)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (dc *DynCommit) RandTestMessage(t *rapid.T) Message {
	chanID := RandChannelID(t)

	da := &DynAck{
		ChanID: chanID,
	}

	dp := &DynPropose{
		ChanID: chanID,
	}

	// Randomly decide which optional fields to include
	includeDustLimit := rapid.Bool().Draw(t, "includeDustLimit")
	includeMaxValueInFlight := rapid.Bool().Draw(
		t, "includeMaxValueInFlight",
	)
	includeChannelReserve := rapid.Bool().Draw(t, "includeChannelReserve")
	includeCsvDelay := rapid.Bool().Draw(t, "includeCsvDelay")
	includeMaxAcceptedHTLCs := rapid.Bool().Draw(
		t, "includeMaxAcceptedHTLCs",
	)
	includeChannelType := rapid.Bool().Draw(t, "includeChannelType")

	// Generate random values for each included field
	if includeDustLimit {
		var rec tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
		val := btcutil.Amount(rapid.Uint32().Draw(t, "dustLimit"))
		rec.Val = tlv.NewBigSizeT(val)
		dp.DustLimit = tlv.SomeRecordT(rec)
	}

	if includeMaxValueInFlight {
		var rec tlv.RecordT[tlv.TlvType2, MilliSatoshi]
		val := MilliSatoshi(rapid.Uint64().Draw(t, "maxValueInFlight"))
		rec.Val = val
		dp.MaxValueInFlight = tlv.SomeRecordT(rec)
	}

	if includeChannelReserve {
		var rec tlv.RecordT[tlv.TlvType6, tlv.BigSizeT[btcutil.Amount]]
		val := btcutil.Amount(rapid.Uint32().Draw(t, "channelReserve"))
		rec.Val = tlv.NewBigSizeT(val)
		dp.ChannelReserve = tlv.SomeRecordT(rec)
	}

	if includeCsvDelay {
		csvDelay := dp.CsvDelay.Zero()
		val := rapid.Uint16().Draw(t, "csvDelay")
		csvDelay.Val = val
		dp.CsvDelay = tlv.SomeRecordT(csvDelay)
	}

	if includeMaxAcceptedHTLCs {
		maxHtlcs := dp.MaxAcceptedHTLCs.Zero()
		maxHtlcs.Val = rapid.Uint16().Draw(t, "maxAcceptedHTLCs")
		dp.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}

	if includeChannelType {
		chanType := dp.ChannelType.Zero()
		chanType.Val = *RandChannelType(t)
		dp.ChannelType = tlv.SomeRecordT(chanType)
	}

	includeLocalNonce := rapid.Bool().Draw(t, "includeLocalNonce")
	if includeLocalNonce {
		nonce := RandMusig2Nonce(t)
		rec := tlv.NewRecordT[tlv.TlvType14](nonce)
		da.LocalNonce = tlv.SomeRecordT(rec)
	}

	// Create a tlv type lists to hold all known records which will be
	// ignored when creating ExtraData records.
	ignoreRecords := fn.NewSet[uint64]()
	for i := range uint64(15) {
		// Ignore known records.
		if i%2 == 0 {
			ignoreRecords.Add(i)
		}
	}
	msg := &DynCommit{
		DynPropose: *dp,
		DynAck:     *da,
	}

	msg.ExtraData = RandExtraOpaqueData(t, ignoreRecords)

	return msg
}

// A compile time check to ensure FundingCreated implements the TestMessage
// interface.
var _ TestMessage = (*FundingCreated)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (f *FundingCreated) RandTestMessage(t *rapid.T) Message {
	var pendingChanID [32]byte
	pendingChanIDBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
		t, "pendingChanID",
	)
	copy(pendingChanID[:], pendingChanIDBytes)

	includePartialSig := rapid.Bool().Draw(t, "includePartialSig")
	var partialSig OptPartialSigWithNonceTLV
	var commitSig Sig

	if includePartialSig {
		sigWithNonce := RandPartialSigWithNonce(t)
		partialSig = MaybePartialSigWithNonce(sigWithNonce)

		// When using partial sig, CommitSig should be empty/blank.
		commitSig = Sig{}
	} else {
		commitSig = RandSignature(t)
	}

	return &FundingCreated{
		PendingChannelID: pendingChanID,
		FundingPoint:     RandOutPoint(t),
		CommitSig:        commitSig,
		PartialSig:       partialSig,
		ExtraData:        RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure FundingSigned implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*FundingSigned)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (f *FundingSigned) RandTestMessage(t *rapid.T) Message {
	usePartialSig := rapid.Bool().Draw(t, "usePartialSig")

	msg := &FundingSigned{
		ChanID:    RandChannelID(t),
		ExtraData: RandExtraOpaqueData(t, nil),
	}

	if usePartialSig {
		sigWithNonce := RandPartialSigWithNonce(t)
		msg.PartialSig = MaybePartialSigWithNonce(sigWithNonce)

		msg.CommitSig = Sig{}
	} else {
		msg.CommitSig = RandSignature(t)
	}

	return msg
}

// A compile time check to ensure GossipTimestampRange implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*GossipTimestampRange)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (g *GossipTimestampRange) RandTestMessage(t *rapid.T) Message {
	var chainHash chainhash.Hash
	hashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "chainHash")
	copy(chainHash[:], hashBytes)

	msg := &GossipTimestampRange{
		ChainHash:      chainHash,
		FirstTimestamp: rapid.Uint32().Draw(t, "firstTimestamp"),
		TimestampRange: rapid.Uint32().Draw(t, "timestampRange"),
		ExtraData:      RandExtraOpaqueData(t, nil),
	}

	includeFirstBlockHeight := rapid.Bool().Draw(
		t, "includeFirstBlockHeight",
	)
	includeBlockRange := rapid.Bool().Draw(t, "includeBlockRange")

	if includeFirstBlockHeight {
		height := rapid.Uint32().Draw(t, "firstBlockHeight")
		msg.FirstBlockHeight = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType2, uint32]{Val: height},
		)
	}

	if includeBlockRange {
		blockRange := rapid.Uint32().Draw(t, "blockRange")
		msg.BlockRange = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType4, uint32]{Val: blockRange},
		)
	}

	return msg
}

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (msg *Init) RandTestMessage(t *rapid.T) Message {
	global := NewRawFeatureVector()
	local := NewRawFeatureVector()

	numGlobalFeatures := rapid.IntRange(0, 20).Draw(t, "numGlobalFeatures")
	for i := 0; i < numGlobalFeatures; i++ {
		bit := FeatureBit(
			rapid.IntRange(0, 100).Draw(
				t, fmt.Sprintf("globalFeatureBit%d", i),
			),
		)
		global.Set(bit)
	}

	numLocalFeatures := rapid.IntRange(0, 20).Draw(t, "numLocalFeatures")
	for i := 0; i < numLocalFeatures; i++ {
		bit := FeatureBit(
			rapid.IntRange(0, 100).Draw(
				t, fmt.Sprintf("localFeatureBit%d", i),
			),
		)
		local.Set(bit)
	}

	ignoreRecords := fn.NewSet[uint64]()

	msg.ExtraData = RandExtraOpaqueData(t, ignoreRecords)

	return NewInitMessage(global, local)
}

// A compile time check to ensure KickoffSig implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*KickoffSig)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (ks *KickoffSig) RandTestMessage(t *rapid.T) Message {
	return &KickoffSig{
		ChanID:    RandChannelID(t),
		Signature: RandSignature(t),
		ExtraData: RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure NodeAnnouncement1 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*NodeAnnouncement1)(nil)

// A compile time check to ensure NodeAnnouncement2 implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*NodeAnnouncement2)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (a *NodeAnnouncement1) RandTestMessage(t *rapid.T) Message {
	// Generate random compressed public key for node ID
	pubKey := RandPubKey(t)
	var nodeID [33]byte
	copy(nodeID[:], pubKey.SerializeCompressed())

	// Generate random RGB color
	rgbColor := color.RGBA{
		R: uint8(rapid.IntRange(0, 255).Draw(t, "rgbR")),
		G: uint8(rapid.IntRange(0, 255).Draw(t, "rgbG")),
		B: uint8(rapid.IntRange(0, 255).Draw(t, "rgbB")),
	}

	return &NodeAnnouncement1{
		Signature: RandSignature(t),
		Features:  RandFeatureVector(t),
		Timestamp: uint32(rapid.IntRange(0, 0x7FFFFFFF).Draw(
			t, "timestamp"),
		),
		NodeID:          nodeID,
		RGBColor:        rgbColor,
		Alias:           RandNodeAlias(t),
		Addresses:       RandNetAddrs(t),
		ExtraOpaqueData: RandExtraOpaqueData(t, nil),
	}
}

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (n *NodeAnnouncement2) RandTestMessage(t *rapid.T) Message {
	features := RandFeatureVector(t)
	blockHeight := uint32(rapid.IntRange(0, 1000000).Draw(t, "blockHeight"))

	// Generate random compressed public key for node ID
	pubKey := RandPubKey(t)
	var nodeID [33]byte
	copy(nodeID[:], pubKey.SerializeCompressed())

	msg := &NodeAnnouncement2{
		Features: tlv.NewRecordT[tlv.TlvType0](*features),
		BlockHeight: tlv.NewPrimitiveRecord[tlv.TlvType2](
			blockHeight,
		),
		NodeID:            tlv.NewPrimitiveRecord[tlv.TlvType4](nodeID),
		ExtraSignedFields: make(map[uint64][]byte),
	}

	msg.Signature.Val = RandSignature(t)
	msg.Signature.Val.ForceSchnorr()

	// Test optional fields one by one
	if rapid.Bool().Draw(t, "includeColor") {
		rgbColor := Color{
			R: uint8(rapid.IntRange(0, 255).Draw(t, "rgbR")),
			G: uint8(rapid.IntRange(0, 255).Draw(t, "rgbG")),
			B: uint8(rapid.IntRange(0, 255).Draw(t, "rgbB")),
		}
		colorRecord := tlv.ZeroRecordT[tlv.TlvType1, Color]()
		colorRecord.Val = rgbColor
		msg.Color = tlv.SomeRecordT(colorRecord)
	}

	if rapid.Bool().Draw(t, "includeAlias") {
		aliasBytes := RandNodeAlias2(t)
		aliasRecord := tlv.ZeroRecordT[tlv.TlvType3, NodeAlias2]()
		aliasRecord.Val = aliasBytes
		msg.Alias = tlv.SomeRecordT(aliasRecord)
	}

	if rapid.Bool().Draw(t, "includeIPV4Addrs") {
		ipv4Addrs := make(IPV4Addrs, 1)
		ip := make(net.IP, 4)
		ip[0] = uint8(rapid.IntRange(1, 223).Draw(t, "ip4_0"))
		ip[1] = uint8(rapid.IntRange(0, 255).Draw(t, "ip4_1"))
		ip[2] = uint8(rapid.IntRange(0, 255).Draw(t, "ip4_2"))
		ip[3] = uint8(rapid.IntRange(1, 254).Draw(t, "ip4_3"))

		ipv4Addrs[0] = &net.TCPAddr{
			IP:   ip,
			Port: rapid.IntRange(1, 65535).Draw(t, "port4"),
		}

		ipv4Record := tlv.ZeroRecordT[tlv.TlvType5, IPV4Addrs]()
		ipv4Record.Val = ipv4Addrs
		msg.IPV4Addrs = tlv.SomeRecordT(ipv4Record)
	}

	if rapid.Bool().Draw(t, "includeIPV6Addrs") {
		ipv6Addrs := make(IPV6Addrs, 1)
		ip := make(net.IP, 16)
		// Generate random IPv6 address.
		for j := 0; j < 16; j++ {
			ip[j] = uint8(rapid.IntRange(0, 255).Draw(
				t, fmt.Sprintf("ip6_%d", j)),
			)
		}

		ipv6Addrs[0] = &net.TCPAddr{
			IP:   ip,
			Port: rapid.IntRange(1, 65535).Draw(t, "port6"),
		}

		ipv6Record := tlv.ZeroRecordT[tlv.TlvType7, IPV6Addrs]()
		ipv6Record.Val = ipv6Addrs
		msg.IPV6Addrs = tlv.SomeRecordT(ipv6Record)
	}

	if rapid.Bool().Draw(t, "includeTorV3Addrs") {
		torV3Addrs := make(TorV3Addrs, 1)
		onionBytes := rapid.SliceOfN(rapid.Byte(), 35, 35).Draw(
			t, "onion",
		)
		onionService := tor.Base32Encoding.EncodeToString(onionBytes) +
			tor.OnionSuffix

		torV3Addrs[0] = &tor.OnionAddr{
			OnionService: onionService,
			Port: rapid.IntRange(1, 65535).Draw(
				t, "torPort",
			),
		}

		torV3Record := tlv.ZeroRecordT[tlv.TlvType9, TorV3Addrs]()
		torV3Record.Val = torV3Addrs
		msg.TorV3Addrs = tlv.SomeRecordT(torV3Record)
	}

	if rapid.Bool().Draw(t, "includeDNSHostName") {
		// Generate a valid DNS hostname.
		hostname := genValidHostname(t)
		port := rapid.Uint16Range(1, 65535).Draw(t, "dnsPort")

		dnsAddr := DNSAddress{
			Hostname: hostname,
			Port:     port,
		}

		dnsRecord := tlv.ZeroRecordT[tlv.TlvType11, DNSAddress]()
		dnsRecord.Val = dnsAddr
		msg.DNSHostName = tlv.SomeRecordT(dnsRecord)
	}

	randRecs, _ := RandSignedRangeRecords(t)
	if len(randRecs) > 0 {
		msg.ExtraSignedFields = ExtraSignedFields(randRecs)
	}

	return msg
}

// A compile time check to ensure OpenChannel implements the TestMessage
// interface.
var _ TestMessage = (*OpenChannel)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (o *OpenChannel) RandTestMessage(t *rapid.T) Message {
	chainHash := RandChainHash(t)
	var hash chainhash.Hash
	copy(hash[:], chainHash[:])

	var pendingChanID [32]byte
	pendingChanIDBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
		t, "pendingChanID",
	)
	copy(pendingChanID[:], pendingChanIDBytes)

	includeChannelType := rapid.Bool().Draw(t, "includeChannelType")
	includeLeaseExpiry := rapid.Bool().Draw(t, "includeLeaseExpiry")
	includeLocalNonce := rapid.Bool().Draw(t, "includeLocalNonce")

	var channelFlags FundingFlag
	if rapid.Bool().Draw(t, "announceChannel") {
		channelFlags |= FFAnnounceChannel
	}

	var localNonce OptMusig2NonceTLV
	if includeLocalNonce {
		nonce := RandMusig2Nonce(t)
		localNonce = tlv.SomeRecordT(
			tlv.NewRecordT[NonceRecordTypeT, Musig2Nonce](nonce),
		)
	}

	var channelType *ChannelType
	if includeChannelType {
		channelType = RandChannelType(t)
	}

	var leaseExpiry *LeaseExpiry
	if includeLeaseExpiry {
		leaseExpiry = RandLeaseExpiry(t)
	}

	return &OpenChannel{
		ChainHash:        hash,
		PendingChannelID: pendingChanID,
		FundingAmount: btcutil.Amount(
			rapid.IntRange(5000, 10000000).Draw(t, "fundingAmount"),
		),
		PushAmount: MilliSatoshi(
			rapid.IntRange(0, 1000000).Draw(t, "pushAmount"),
		),
		DustLimit: btcutil.Amount(
			rapid.IntRange(100, 1000).Draw(t, "dustLimit"),
		),
		MaxValueInFlight: MilliSatoshi(
			rapid.IntRange(10000, 1000000).Draw(
				t, "maxValueInFlight",
			),
		),
		ChannelReserve: btcutil.Amount(
			rapid.IntRange(1000, 10000).Draw(t, "channelReserve"),
		),
		HtlcMinimum: MilliSatoshi(
			rapid.IntRange(1, 1000).Draw(t, "htlcMinimum"),
		),
		FeePerKiloWeight: uint32(
			rapid.IntRange(250, 10000).Draw(t, "feePerKw"),
		),
		CsvDelay: uint16(
			rapid.IntRange(144, 1000).Draw(t, "csvDelay"),
		),
		MaxAcceptedHTLCs: uint16(
			rapid.IntRange(10, 500).Draw(t, "maxAcceptedHTLCs"),
		),
		FundingKey:            RandPubKey(t),
		RevocationPoint:       RandPubKey(t),
		PaymentPoint:          RandPubKey(t),
		DelayedPaymentPoint:   RandPubKey(t),
		HtlcPoint:             RandPubKey(t),
		FirstCommitmentPoint:  RandPubKey(t),
		ChannelFlags:          channelFlags,
		UpfrontShutdownScript: RandDeliveryAddress(t),
		ChannelType:           channelType,
		LeaseExpiry:           leaseExpiry,
		LocalNonce:            localNonce,
		ExtraData:             RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure Ping implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Ping)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (p *Ping) RandTestMessage(t *rapid.T) Message {
	numPongBytes := uint16(rapid.IntRange(0, int(MaxPongBytes)).Draw(
		t, "numPongBytes"),
	)

	// Generate padding bytes (but keeping within allowed message size)
	// MaxMsgBody - 2 (for NumPongBytes) - 2 (for padding length)
	maxPaddingLen := MaxMsgBody - 4
	paddingLen := rapid.IntRange(0, maxPaddingLen).Draw(
		t, "paddingLen",
	)
	padding := make(PingPayload, paddingLen)

	// Fill padding with random bytes
	for i := 0; i < paddingLen; i++ {
		padding[i] = byte(rapid.IntRange(0, 255).Draw(
			t, fmt.Sprintf("paddingByte%d", i)),
		)
	}

	return &Ping{
		NumPongBytes: numPongBytes,
		PaddingBytes: padding,
	}
}

// A compile time check to ensure Pong implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Pong)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (p *Pong) RandTestMessage(t *rapid.T) Message {
	payloadLen := rapid.IntRange(0, 1000).Draw(t, "pongPayloadLength")
	payload := rapid.SliceOfN(rapid.Byte(), payloadLen, payloadLen).Draw(
		t, "pongPayload",
	)

	return &Pong{
		PongBytes: payload,
	}
}

// A compile time check to ensure QueryChannelRange implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*QueryChannelRange)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (q *QueryChannelRange) RandTestMessage(t *rapid.T) Message {
	msg := &QueryChannelRange{
		FirstBlockHeight: uint32(rapid.IntRange(0, 1000000).Draw(
			t, "firstBlockHeight"),
		),
		NumBlocks: uint32(rapid.IntRange(1, 10000).Draw(
			t, "numBlocks"),
		),
		ExtraData: RandExtraOpaqueData(t, nil),
	}

	// Generate chain hash
	chainHash := RandChainHash(t)
	var chainHashObj chainhash.Hash
	copy(chainHashObj[:], chainHash[:])
	msg.ChainHash = chainHashObj

	// Randomly include QueryOptions
	if rapid.Bool().Draw(t, "includeQueryOptions") {
		queryOptions := &QueryOptions{}
		*queryOptions = QueryOptions(*RandFeatureVector(t))
		msg.QueryOptions = queryOptions
	}

	return msg
}

// A compile time check to ensure QueryShortChanIDs implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*QueryShortChanIDs)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (q *QueryShortChanIDs) RandTestMessage(t *rapid.T) Message {
	var chainHash chainhash.Hash
	hashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "chainHash")
	copy(chainHash[:], hashBytes)

	encodingType := EncodingSortedPlain
	if rapid.Bool().Draw(t, "useZlibEncoding") {
		encodingType = EncodingSortedZlib
	}

	msg := &QueryShortChanIDs{
		ChainHash:    chainHash,
		EncodingType: encodingType,
		ExtraData:    RandExtraOpaqueData(t, nil),
		noSort:       false,
	}

	numIDs := rapid.IntRange(2, 20).Draw(t, "numShortChanIDs")

	// Generate sorted short channel IDs.
	shortChanIDs := make([]ShortChannelID, numIDs)
	for i := 0; i < numIDs; i++ {
		shortChanIDs[i] = RandShortChannelID(t)

		// Ensure they're properly sorted.
		if i > 0 && shortChanIDs[i].ToUint64() <=
			shortChanIDs[i-1].ToUint64() {

			// Ensure this ID is larger than the previous one.
			shortChanIDs[i] = NewShortChanIDFromInt(
				shortChanIDs[i-1].ToUint64() + 1,
			)
		}
	}

	msg.ShortChanIDs = shortChanIDs

	return msg
}

// A compile time check to ensure ReplyChannelRange implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ReplyChannelRange)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ReplyChannelRange) RandTestMessage(t *rapid.T) Message {
	msg := &ReplyChannelRange{
		FirstBlockHeight: uint32(rapid.IntRange(0, 1000000).Draw(
			t, "firstBlockHeight"),
		),
		NumBlocks: uint32(rapid.IntRange(1, 10000).Draw(
			t, "numBlocks"),
		),
		Complete: uint8(rapid.IntRange(0, 1).Draw(t, "complete")),
		EncodingType: QueryEncoding(
			rapid.IntRange(0, 1).Draw(t, "encodingType"),
		),
		ExtraData: RandExtraOpaqueData(t, nil),
	}

	msg.ChainHash = RandChainHash(t)

	numShortChanIDs := rapid.IntRange(0, 20).Draw(t, "numShortChanIDs")
	if numShortChanIDs == 0 {
		return msg
	}

	scidSet := fn.NewSet[ShortChannelID]()
	scids := make([]ShortChannelID, numShortChanIDs)
	for i := 0; i < numShortChanIDs; i++ {
		scid := RandShortChannelID(t)
		for scidSet.Contains(scid) {
			scid = RandShortChannelID(t)
		}

		scids[i] = scid

		scidSet.Add(scid)
	}

	// Make sure there're no duplicates.
	msg.ShortChanIDs = scids

	if rapid.Bool().Draw(t, "includeTimestamps") && numShortChanIDs > 0 {
		msg.Timestamps = make(Timestamps, numShortChanIDs)
		for i := 0; i < numShortChanIDs; i++ {
			msg.Timestamps[i] = ChanUpdateTimestamps{
				Timestamp1: uint32(rapid.IntRange(0, math.MaxInt32).Draw(t, fmt.Sprintf("timestamp-1-%d", i))), //nolint:ll
				Timestamp2: uint32(rapid.IntRange(0, math.MaxInt32).Draw(t, fmt.Sprintf("timestamp-2-%d", i))), //nolint:ll
			}
		}
	}

	return msg
}

// A compile time check to ensure ReplyShortChanIDsEnd implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*ReplyShortChanIDsEnd)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *ReplyShortChanIDsEnd) RandTestMessage(t *rapid.T) Message {
	var chainHash chainhash.Hash
	hashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "chainHash")
	copy(chainHash[:], hashBytes)

	complete := uint8(rapid.IntRange(0, 1).Draw(t, "complete"))

	return &ReplyShortChanIDsEnd{
		ChainHash: chainHash,
		Complete:  complete,
		ExtraData: RandExtraOpaqueData(t, nil),
	}
}

// RandTestMessage returns a RevokeAndAck message populated with random data.
//
// This is part of the TestMessage interface.
func (c *RevokeAndAck) RandTestMessage(t *rapid.T) Message {
	msg := NewRevokeAndAck()

	var chanID ChannelID
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "channelID")
	copy(chanID[:], bytes)
	msg.ChanID = chanID

	revBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "revocation")
	copy(msg.Revocation[:], revBytes)

	msg.NextRevocationKey = RandPubKey(t)

	if rapid.Bool().Draw(t, "includeLocalNonce") {
		var nonce Musig2Nonce
		nonceBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			t, "nonce",
		)
		copy(nonce[:], nonceBytes)

		msg.LocalNonce = tlv.SomeRecordT(
			tlv.NewRecordT[NonceRecordTypeT, Musig2Nonce](nonce),
		)
	}

	return msg
}

// A compile-time check to ensure Shutdown implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Shutdown)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (s *Shutdown) RandTestMessage(t *rapid.T) Message {
	// Generate random delivery address
	// First decide the address type (P2PKH, P2SH, P2WPKH, P2WSH, P2TR)
	addrType := rapid.IntRange(0, 4).Draw(t, "addrType")

	// Generate random address length based on type
	var addrLen int
	switch addrType {
	// P2PKH
	case 0:
		addrLen = 25
	// P2SH
	case 1:
		addrLen = 23
	// P2WPKH
	case 2:
		addrLen = 22
	// P2WSH
	case 3:
		addrLen = 34
	// P2TR
	case 4:
		addrLen = 34
	}

	addr := rapid.SliceOfN(rapid.Byte(), addrLen, addrLen).Draw(
		t, "address",
	)

	// Randomly decide whether to include a shutdown nonce
	includeNonce := rapid.Bool().Draw(t, "includeNonce")
	var shutdownNonce ShutdownNonceTLV

	if includeNonce {
		shutdownNonce = SomeShutdownNonce(RandMusig2Nonce(t))
	}

	cr, _ := RandCustomRecords(t, nil)

	return &Shutdown{
		ChannelID:     RandChannelID(t),
		Address:       addr,
		ShutdownNonce: shutdownNonce,
		CustomRecords: cr,
	}
}

// A compile time check to ensure Stfu implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Stfu)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (s *Stfu) RandTestMessage(t *rapid.T) Message {
	m := &Stfu{
		ChanID:    RandChannelID(t),
		Initiator: rapid.Bool().Draw(t, "initiator"),
	}

	extraData := RandExtraOpaqueData(t, nil)
	if len(extraData) > 0 {
		m.ExtraData = extraData
	}

	return m
}

// A compile time check to ensure UpdateAddHTLC implements the
// lnwire.TestMessage interface.
var _ TestMessage = (*UpdateAddHTLC)(nil)

// RandTestMessage returns an UpdateAddHTLC message populated with random data.
//
// This is part of the TestMessage interface.
func (c *UpdateAddHTLC) RandTestMessage(t *rapid.T) Message {
	msg := &UpdateAddHTLC{
		ChanID: RandChannelID(t),
		ID:     rapid.Uint64().Draw(t, "id"),
		Amount: MilliSatoshi(rapid.Uint64().Draw(t, "amount")),
		Expiry: rapid.Uint32().Draw(t, "expiry"),
	}

	hashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "paymentHash")
	copy(msg.PaymentHash[:], hashBytes)

	onionBytes := rapid.SliceOfN(
		rapid.Byte(), OnionPacketSize, OnionPacketSize,
	).Draw(t, "onionBlob")
	copy(msg.OnionBlob[:], onionBytes)

	numRecords := rapid.IntRange(0, 5).Draw(t, "numRecords")
	if numRecords > 0 {
		msg.CustomRecords, _ = RandCustomRecords(t, nil)
	}

	// 50/50 chance to add a blinding point
	if rapid.Bool().Draw(t, "includeBlindingPoint") {
		pubKey := RandPubKey(t)

		msg.BlindingPoint = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[BlindingPointTlvType](pubKey),
		)
	}

	return msg
}

// A compile time check to ensure UpdateFailHTLC implements the TestMessage
// interface.
var _ TestMessage = (*UpdateFailHTLC)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *UpdateFailHTLC) RandTestMessage(t *rapid.T) Message {
	return &UpdateFailHTLC{
		ChanID:    RandChannelID(t),
		ID:        rapid.Uint64().Draw(t, "id"),
		Reason:    RandOpaqueReason(t),
		ExtraData: RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure UpdateFailMalformedHTLC implements the
// TestMessage interface.
var _ TestMessage = (*UpdateFailMalformedHTLC)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *UpdateFailMalformedHTLC) RandTestMessage(t *rapid.T) Message {
	return &UpdateFailMalformedHTLC{
		ChanID:       RandChannelID(t),
		ID:           rapid.Uint64().Draw(t, "id"),
		ShaOnionBlob: RandSHA256Hash(t),
		FailureCode:  RandFailCode(t),
		ExtraData:    RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure UpdateFee implements the TestMessage
// interface.
var _ TestMessage = (*UpdateFee)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *UpdateFee) RandTestMessage(t *rapid.T) Message {
	return &UpdateFee{
		ChanID:    RandChannelID(t),
		FeePerKw:  uint32(rapid.IntRange(1, 10000).Draw(t, "feePerKw")),
		ExtraData: RandExtraOpaqueData(t, nil),
	}
}

// A compile time check to ensure UpdateFulfillHTLC implements the TestMessage
// interface.
var _ TestMessage = (*UpdateFulfillHTLC)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *UpdateFulfillHTLC) RandTestMessage(t *rapid.T) Message {
	msg := &UpdateFulfillHTLC{
		ChanID:          RandChannelID(t),
		ID:              rapid.Uint64().Draw(t, "id"),
		PaymentPreimage: RandPaymentPreimage(t),
	}

	cr, ignoreRecords := RandCustomRecords(t, nil)
	msg.CustomRecords = cr

	randData := RandExtraOpaqueData(t, ignoreRecords)
	if len(randData) > 0 {
		msg.ExtraData = randData
	}

	return msg
}

// A compile time check to ensure Warning implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Warning)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *Warning) RandTestMessage(t *rapid.T) Message {
	msg := &Warning{
		ChanID: RandChannelID(t),
	}

	useASCII := rapid.Bool().Draw(t, "useASCII")
	if useASCII {
		length := rapid.IntRange(1, 100).Draw(t, "warningDataLength")
		data := make([]byte, length)
		for i := 0; i < length; i++ {
			data[i] = byte(
				rapid.IntRange(32, 126).Draw(
					t, fmt.Sprintf("warningDataByte-%d", i),
				),
			)
		}
		msg.Data = data
	} else {
		length := rapid.IntRange(1, 100).Draw(t, "warningDataLength")
		msg.Data = rapid.SliceOfN(rapid.Byte(), length, length).Draw(
			t, "warningData",
		)
	}

	return msg
}

// A compile time check to ensure Error implements the lnwire.TestMessage
// interface.
var _ TestMessage = (*Error)(nil)

// RandTestMessage populates the message with random data suitable for testing.
// It uses the rapid testing framework to generate random values.
//
// This is part of the TestMessage interface.
func (c *Error) RandTestMessage(t *rapid.T) Message {
	msg := &Error{
		ChanID: RandChannelID(t),
	}

	useASCII := rapid.Bool().Draw(t, "useASCII")
	if useASCII {
		length := rapid.IntRange(1, 100).Draw(t, "errorDataLength")
		data := make([]byte, length)
		for i := 0; i < length; i++ {
			data[i] = byte(
				rapid.IntRange(32, 126).Draw(
					t, fmt.Sprintf("errorDataByte-%d", i),
				),
			)
		}
		msg.Data = data
	} else {
		// Generate random binary data
		length := rapid.IntRange(1, 100).Draw(t, "errorDataLength")
		msg.Data = rapid.SliceOfN(
			rapid.Byte(), length, length,
		).Draw(t, "errorData")
	}

	return msg
}

// genValidHostname generates a random valid hostname according to BOLT #7
// rules.
func genValidHostname(t *rapid.T) string {
	// Valid characters: a-z, A-Z, 0-9, -, .
	validChars := "abcdefghijklmnopqrstuvwxyzABCDE" +
		"FGHIJKLMNOPQRSTUVWXYZ0123456789-."

	// Generate hostname length between 1 and 255 characters.
	length := rapid.IntRange(1, 255).Draw(t, "hostname_length")

	hostname := make([]byte, length)
	for i := 0; i < length; i++ {
		charIndex := rapid.IntRange(0, len(validChars)-1).Draw(
			t, fmt.Sprintf("char_%d", i),
		)
		hostname[i] = validChars[charIndex]
	}

	return string(hostname)
}
