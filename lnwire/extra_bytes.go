package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/lightningnetwork/lnd/tlv"
)

// ExtraOpaqueData is the set of data that was appended to this message, some
// of which we may not actually know how to iterate or parse. By holding onto
// this data, we ensure that we're able to properly validate the set of
// signatures that cover these new fields, and ensure we're able to make
// upgrades to the network in a forwards compatible manner.
type ExtraOpaqueData []byte

// Encode attempts to encode the raw extra bytes into the passed io.Writer.
func (e *ExtraOpaqueData) Encode(w io.Writer) error {
	eBytes := []byte((*e)[:])
	if err := WriteElements(w, eBytes); err != nil {
		return err
	}

	return nil
}

// Decode attempts to unpack the raw bytes encoded in the passed io.Reader as a
// set of extra opaque data.
func (e *ExtraOpaqueData) Decode(r io.Reader) error {
	// First, we'll attempt to read a set of bytes contained within the
	// passed io.Reader (if any exist).
	rawBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	// If we _do_ have some bytes, then we'll swap out our backing pointer.
	// This ensures that any struct that embeds this type will properly
	// store the bytes once this method exits.
	if len(rawBytes) > 0 {
		*e = ExtraOpaqueData(rawBytes)
	} else {
		*e = make([]byte, 0)
	}

	return nil
}

// PackRecords attempts to encode the set of tlv records into the target
// ExtraOpaqueData instance. The records will be encoded as a raw TLV stream
// and stored within the backing slice pointer.
func (e *ExtraOpaqueData) PackRecords(recordProducers ...tlv.RecordProducer) error {
	// First, assemble all the records passed in in series.
	records := make([]tlv.Record, 0, len(recordProducers))
	for _, producer := range recordProducers {
		records = append(records, producer.Record())
	}

	// Ensure that the set of records are sorted before we encode them into
	// the stream, to ensure they're canonical.
	tlv.SortRecords(records)

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	var extraBytesWriter bytes.Buffer
	if err := tlvStream.Encode(&extraBytesWriter); err != nil {
		return err
	}

	*e = ExtraOpaqueData(extraBytesWriter.Bytes())

	return nil
}

// ExtractRecords attempts to decode any types in the internal raw bytes as if
// it were a tlv stream. The set of raw parsed types is returned, and any
// passed records (if found in the stream) will be parsed into the proper
// tlv.Record.
func (e *ExtraOpaqueData) ExtractRecords(recordProducers ...tlv.RecordProducer) (
	tlv.TypeMap, error) {

	// First, assemble all the records passed in in series.
	records := make([]tlv.Record, 0, len(recordProducers))
	for _, producer := range recordProducers {
		records = append(records, producer.Record())
	}

	extraBytesReader := bytes.NewReader(*e)

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	return tlvStream.DecodeWithParsedTypes(extraBytesReader)
}

// EncodeMessageExtraData encodes the given recordProducers into the given
// extraData.
func EncodeMessageExtraData(extraData *ExtraOpaqueData,
	recordProducers ...tlv.RecordProducer) error {

	// Treat extraData as a mutable reference.
	if extraData == nil {
		return fmt.Errorf("extra data cannot be nil")
	}

	// Pack in the series of TLV records into this message. The order we
	// pass them in doesn't matter, as the method will ensure that things
	// are all properly sorted.
	return extraData.PackRecords(recordProducers...)
}
