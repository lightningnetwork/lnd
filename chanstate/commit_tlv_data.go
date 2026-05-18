package chanstate

import (
	"io"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// commitTlvData stores all the optional data that may be stored as a TLV stream
// at the _end_ of the normal serialized commit on disk.
type commitTlvData struct {
	// customBlob is a custom blob that may store extra data for custom
	// channels.
	customBlob tlv.OptionalRecordT[tlv.TlvType1, tlv.Blob]
}

// encode encodes the aux data into the passed io.Writer.
func (c *commitTlvData) encode(w io.Writer) error {
	var tlvRecords []tlv.Record
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType1, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode attempts to decode the aux data from the passed io.Reader.
func (c *commitTlvData) decode(r io.Reader) error {
	blob := c.customBlob.Zero()

	tlvStream, err := tlv.NewStream(
		blob.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}

	return nil
}

// DecodeCommitTlvData decodes and applies auxiliary TLV data to a commitment.
func DecodeCommitTlvData(r io.Reader, c *ChannelCommitment) error {
	var auxData commitTlvData
	if err := auxData.decode(r); err != nil {
		return err
	}

	amendCommitTlvData(c, auxData)

	return nil
}

// EncodeCommitTlvData extracts and encodes auxiliary TLV data from a
// commitment.
func EncodeCommitTlvData(w io.Writer, c *ChannelCommitment) error {
	auxData := extractCommitTlvData(c)
	return auxData.encode(w)
}

// amendCommitTlvData updates the commitment with the given auxiliary TLV data.
func amendCommitTlvData(c *ChannelCommitment, auxData commitTlvData) {
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		c.CustomBlob = fn.Some(blob)
	})
}

// extractCommitTlvData creates a new commitTlvData from the given commitment.
func extractCommitTlvData(c *ChannelCommitment) commitTlvData {
	var auxData commitTlvData

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](blob),
		)
	})

	return auxData
}
