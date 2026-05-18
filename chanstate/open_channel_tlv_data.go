package chanstate

import (
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// openChannelTlvData houses the new data fields that are stored for each
// channel in a TLV stream within the root bucket. This is stored as a TLV
// stream appended to the existing hard-coded fields in the channel's root
// bucket. New fields being added to the channel state should be added here.
//
// NOTE: This struct is used for serialization purposes only and its fields
// should be accessed via the OpenChannel struct while in memory.
type openChannelTlvData struct {
	// revokeKeyLoc is the key locator for the revocation key.
	revokeKeyLoc tlv.RecordT[tlv.TlvType1, keyLocRecord]

	// initialLocalBalance is the initial local balance of the channel.
	initialLocalBalance tlv.RecordT[tlv.TlvType2, uint64]

	// initialRemoteBalance is the initial remote balance of the channel.
	initialRemoteBalance tlv.RecordT[tlv.TlvType3, uint64]

	// realScid is the real short channel ID of the channel corresponding to
	// the on-chain outpoint.
	realScid tlv.RecordT[tlv.TlvType4, lnwire.ShortChannelID]

	// memo is an optional text field that gives context to the user about
	// the channel.
	memo tlv.OptionalRecordT[tlv.TlvType5, []byte]

	// tapscriptRoot is the optional Tapscript root the channel funding
	// output commits to.
	tapscriptRoot tlv.OptionalRecordT[tlv.TlvType6, [32]byte]

	// customBlob is an optional TLV encoded blob of data representing
	// custom channel funding information.
	customBlob tlv.OptionalRecordT[tlv.TlvType7, tlv.Blob]

	// confirmationHeight records the block height at which the funding
	// transaction was first confirmed.
	confirmationHeight tlv.RecordT[tlv.TlvType8, uint32]

	// closeConfirmationHeight records the block height at which the closing
	// transaction was first confirmed. This is used to calculate the
	// remaining confirmations until the channel is considered fully closed.
	// Note: if not set, it means either the channel has not been
	// closed yet, or it was closed before this field was introduced.
	closeConfirmationHeight tlv.OptionalRecordT[tlv.TlvType9, uint32]
}

// encode serializes the openChannelTlvData to the given io.Writer.
func (c *openChannelTlvData) encode(w io.Writer) error {
	tlvRecords := []tlv.Record{
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		c.confirmationHeight.Record(),
	}
	c.memo.WhenSome(func(memo tlv.RecordT[tlv.TlvType5, []byte]) {
		tlvRecords = append(tlvRecords, memo.Record())
	})
	c.tapscriptRoot.WhenSome(
		func(root tlv.RecordT[tlv.TlvType6, [32]byte]) {
			tlvRecords = append(tlvRecords, root.Record())
		},
	)
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType7, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})
	c.closeConfirmationHeight.WhenSome(
		func(h tlv.RecordT[tlv.TlvType9, uint32]) {
			tlvRecords = append(tlvRecords, h.Record())
		},
	)

	tlv.SortRecords(tlvRecords)

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode deserializes the openChannelTlvData from the given io.Reader.
func (c *openChannelTlvData) decode(r io.Reader) error {
	memo := c.memo.Zero()
	tapscriptRoot := c.tapscriptRoot.Zero()
	blob := c.customBlob.Zero()
	closeConfHeight := c.closeConfirmationHeight.Zero()

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		memo.Record(),
		tapscriptRoot.Record(),
		blob.Record(),
		c.confirmationHeight.Record(),
		closeConfHeight.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[memo.TlvType()]; ok {
		c.memo = tlv.SomeRecordT(memo)
	}
	if _, ok := tlvs[tapscriptRoot.TlvType()]; ok {
		c.tapscriptRoot = tlv.SomeRecordT(tapscriptRoot)
	}
	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}
	if _, ok := tlvs[closeConfHeight.TlvType()]; ok {
		c.closeConfirmationHeight = tlv.SomeRecordT(closeConfHeight)
	}

	return nil
}

// DecodeOpenChannelTlvData decodes and applies auxiliary TLV data to an open
// channel.
func DecodeOpenChannelTlvData(r io.Reader, channel *OpenChannel) error {
	var auxData openChannelTlvData
	if err := auxData.decode(r); err != nil {
		return err
	}

	amendOpenChannelTlvData(channel, auxData)

	return nil
}

// EncodeOpenChannelTlvData extracts and encodes auxiliary TLV data from an open
// channel.
func EncodeOpenChannelTlvData(w io.Writer, channel *OpenChannel) error {
	auxData := extractOpenChannelTlvData(channel)
	return auxData.encode(w)
}

// amendOpenChannelTlvData updates the channel with the given auxiliary TLV
// data.
func amendOpenChannelTlvData(channel *OpenChannel, auxData openChannelTlvData) {
	channel.RevocationKeyLocator = auxData.revokeKeyLoc.Val.KeyLocator
	channel.InitialLocalBalance = lnwire.MilliSatoshi(
		auxData.initialLocalBalance.Val,
	)
	channel.InitialRemoteBalance = lnwire.MilliSatoshi(
		auxData.initialRemoteBalance.Val,
	)
	channel.SetConfirmedScidForStore(auxData.realScid.Val)
	channel.ConfirmationHeight = auxData.confirmationHeight.Val

	auxData.memo.WhenSomeV(func(memo []byte) {
		channel.Memo = memo
	})
	auxData.tapscriptRoot.WhenSomeV(func(h [32]byte) {
		channel.TapscriptRoot = fn.Some[chainhash.Hash](h)
	})
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		channel.CustomBlob = fn.Some(blob)
	})
	auxData.closeConfirmationHeight.WhenSomeV(func(h uint32) {
		channel.CloseConfirmationHeight = fn.Some(h)
	})
}

// extractOpenChannelTlvData creates a new openChannelTlvData from the given
// channel.
func extractOpenChannelTlvData(channel *OpenChannel) openChannelTlvData {
	auxData := openChannelTlvData{
		revokeKeyLoc: tlv.NewRecordT[tlv.TlvType1](
			keyLocRecord{channel.RevocationKeyLocator},
		),
		initialLocalBalance: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint64(channel.InitialLocalBalance),
		),
		initialRemoteBalance: tlv.NewPrimitiveRecord[tlv.TlvType3](
			uint64(channel.InitialRemoteBalance),
		),
		realScid: tlv.NewRecordT[tlv.TlvType4](
			channel.ConfirmedScidForStore(),
		),
		confirmationHeight: tlv.NewPrimitiveRecord[tlv.TlvType8](
			channel.ConfirmationHeight,
		),
	}

	if len(channel.Memo) != 0 {
		auxData.memo = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5](channel.Memo),
		)
	}
	channel.TapscriptRoot.WhenSome(func(h chainhash.Hash) {
		auxData.tapscriptRoot = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6, [32]byte](h),
		)
	})
	channel.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType7](blob),
		)
	})
	channel.CloseConfirmationHeight.WhenSome(func(h uint32) {
		auxData.closeConfirmationHeight = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType9](h),
		)
	})

	return auxData
}
