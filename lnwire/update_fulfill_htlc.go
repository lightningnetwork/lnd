package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// UpdateFulfillHTLC is sent by Alice to Bob when she wishes to settle a
// particular HTLC referenced by its HTLCKey within a specific active channel
// referenced by ChannelPoint.  A subsequent CommitSig message will be sent by
// Alice to "lock-in" the removal of the specified HTLC, possible containing a
// batch signature covering several settled HTLC's.
type UpdateFulfillHTLC struct {
	// ChanID references an active channel which holds the HTLC to be
	// settled.
	ChanID ChannelID

	// ID denotes the exact HTLC stage within the receiving node's
	// commitment transaction to be removed.
	ID uint64

	// PaymentPreimage is the R-value preimage required to fully settle an
	// HTLC.
	PaymentPreimage [32]byte

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewUpdateFulfillHTLC returns a new empty UpdateFulfillHTLC.
func NewUpdateFulfillHTLC(chanID ChannelID, id uint64,
	preimage [32]byte) *UpdateFulfillHTLC {

	return &UpdateFulfillHTLC{
		ChanID:          chanID,
		ID:              id,
		PaymentPreimage: preimage,
	}
}

// A compile time check to ensure UpdateFulfillHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateFulfillHTLC)(nil)

// Decode deserializes a serialized UpdateFulfillHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) Decode(r io.Reader, pver uint32) error {
	// msgExtraData is a temporary variable used to read the message extra
	// data field from the reader.
	var msgExtraData ExtraOpaqueData

	if err := ReadElements(r,
		&c.ChanID,
		&c.ID,
		c.PaymentPreimage[:],
		&msgExtraData,
	); err != nil {
		return err
	}

	// Extract TLV records from the message extra data field.
	extraDataTlvMap, err := msgExtraData.ExtractRecords()
	if err != nil {
		return err
	}

	// Any records from the extra data TLV map which are in the custom
	// records TLV type range will be included in the custom records field
	// and removed from the extra data field.
	customRecordsTlvMap := make(tlv.TypeMap, len(extraDataTlvMap))
	for k, v := range extraDataTlvMap {
		// Skip records that are not in the custom records TLV type
		// range.
		if k < MinCustomRecordsTlvType {
			continue
		}

		// Include the record in the custom records map.
		customRecordsTlvMap[k] = v

		// Now that the record is included in the custom records map,
		// we can remove it from the extra data TLV map.
		delete(extraDataTlvMap, k)
	}

	// Set the custom records field to the TLV record map.
	customRecords, err := NewCustomRecordsFromTlvTypeMap(
		customRecordsTlvMap,
	)
	if err != nil {
		return err
	}
	c.CustomRecords = customRecords

	// Set custom records to nil if we didn't parse anything out of it so
	// that we can use assert.Equal in tests.
	if len(customRecordsTlvMap) == 0 {
		c.CustomRecords = nil
	}

	// Set extra data to nil if we didn't parse anything out of it so that
	// we can use assert.Equal in tests.
	if len(extraDataTlvMap) == 0 {
		c.ExtraData = nil
		return nil
	}

	// Encode the remaining records back into the extra data field. These
	// records are not in the custom records TLV type range and do not
	// have associated fields in the UpdateFulfillHTLC struct.
	c.ExtraData, err = NewExtraOpaqueDataFromTlvTypeMap(extraDataTlvMap)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target UpdateFulfillHTLC into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteUint64(w, c.ID); err != nil {
		return err
	}

	if err := WriteBytes(w, c.PaymentPreimage[:]); err != nil {
		return err
	}

	// Construct a slice of all the records that we should include in the
	// message extra data field. We will start by including any records from
	// the extra data field.
	msgExtraDataRecords, err := c.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	// Include custom records in the extra data wire field if they are
	// present. Ensure that the custom records are validated before encoding
	// them.
	if err := c.CustomRecords.Validate(); err != nil {
		return fmt.Errorf("custom records validation error: %w", err)
	}

	// Extend the message extra data records slice with TLV records from the
	// custom records field.
	customTlvRecords := c.CustomRecords.RecordProducers()
	msgExtraDataRecords = append(msgExtraDataRecords, customTlvRecords...)

	// We will now construct the message extra data field that will be
	// encoded into the byte writer.
	var msgExtraData ExtraOpaqueData
	if err := msgExtraData.PackRecords(msgExtraDataRecords...); err != nil {
		return err
	}

	return WriteBytes(w, msgExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) MsgType() MessageType {
	return MsgUpdateFulfillHTLC
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *UpdateFulfillHTLC) TargetChanID() ChannelID {
	return c.ChanID
}
