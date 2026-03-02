package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// ExtraOpaqueData is the set of data that was appended to this message, some
// of which we may not actually know how to iterate or parse. By holding onto
// this data, we ensure that we're able to properly validate the set of
// signatures that cover these new fields, and ensure we're able to make
// upgrades to the network in a forwards compatible manner.
type ExtraOpaqueData []byte

// NewExtraOpaqueData creates a new ExtraOpaqueData instance from a tlv.TypeMap.
func NewExtraOpaqueData(tlvMap tlv.TypeMap) (ExtraOpaqueData, error) {
	// If the tlv map is empty, we'll want to mirror the behavior of
	// decoding an empty extra opaque data field (see Decode method).
	if len(tlvMap) == 0 {
		return make([]byte, 0), nil
	}

	// Convert the TLV map into a slice of records.
	records := TlvMapToRecords(tlvMap)

	// Encode the records into the extra data byte slice.
	return EncodeRecords(records)
}

// Encode attempts to encode the raw extra bytes into the passed io.Writer.
func (e *ExtraOpaqueData) Encode(w *bytes.Buffer) error {
	eBytes := []byte((*e)[:])
	if err := WriteBytes(w, eBytes); err != nil {
		return err
	}

	return nil
}

// Decode attempts to unpack the raw bytes encoded in the passed-in io.Reader as
// a set of extra opaque data.
func (e *ExtraOpaqueData) Decode(r io.Reader) error {
	// First, we'll attempt to read a set of bytes contained within the
	// passed io.Reader (if any exist).
	rawBytes, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	// If we _do_ have some bytes, then we'll swap out our backing pointer.
	// This ensures that any struct that embeds this type will properly
	// store the bytes once this method exits.
	if len(rawBytes) > 0 {
		*e = rawBytes
	} else {
		*e = make([]byte, 0)
	}

	return nil
}

// ValidateTLV checks that the raw bytes that make up the ExtraOpaqueData
// instance are a valid TLV stream.
func (e *ExtraOpaqueData) ValidateTLV() error {
	// There is nothing to validate if the ExtraOpaqueData is nil or empty.
	if e == nil || len(*e) == 0 {
		return nil
	}

	tlvStream, err := tlv.NewStream()
	if err != nil {
		return err
	}

	// Ensure that the TLV stream is valid by attempting to decode it.
	_, err = tlvStream.DecodeWithParsedTypesP2P(bytes.NewReader(*e))
	if err != nil {
		return fmt.Errorf("invalid TLV stream: %w: %v", err, *e)
	}

	return nil
}

// PackRecords attempts to encode the set of tlv records into the target
// ExtraOpaqueData instance. The records will be encoded as a raw TLV stream
// and stored within the backing slice pointer.
func (e *ExtraOpaqueData) PackRecords(
	recordProducers ...tlv.RecordProducer) error {

	// Assemble all the records passed in series, then encode them.
	records := ProduceRecordsSorted(recordProducers...)
	encoded, err := EncodeRecords(records)
	if err != nil {
		return err
	}

	*e = encoded

	return nil
}

// ExtractRecords attempts to decode any types in the internal raw bytes as if
// it were a tlv stream. The set of raw parsed types is returned, and any
// passed records (if found in the stream) will be parsed into the proper
// tlv.Record.
func (e *ExtraOpaqueData) ExtractRecords(
	recordProducers ...tlv.RecordProducer) (tlv.TypeMap, error) {

	// First, assemble all the records passed in series.
	records := ProduceRecordsSorted(recordProducers...)
	extraBytesReader := bytes.NewReader(*e)

	// Since ExtraOpaqueData is provided by a potentially malicious peer,
	// pass it into the P2P decoding variant.
	return DecodeRecordsP2P(extraBytesReader, records...)
}

// RecordProducers parses ExtraOpaqueData into a slice of TLV record producers
// by interpreting it as a TLV map.
func (e *ExtraOpaqueData) RecordProducers() ([]tlv.RecordProducer, error) {
	var recordProducers []tlv.RecordProducer

	// If the instance is nil or empty, return an empty slice.
	if e == nil || len(*e) == 0 {
		return recordProducers, nil
	}

	// Parse the extra opaque data as a TLV map.
	tlvMap, err := e.ExtractRecords()
	if err != nil {
		return nil, err
	}

	// Convert the TLV map into a slice of record producers.
	records := TlvMapToRecords(tlvMap)

	return RecordsAsProducers(records), nil
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

// ParseAndExtractCustomRecords parses the given extra data into the passed-in
// records, then returns any remaining records split into custom records and
// extra data.
func ParseAndExtractCustomRecords(allExtraData ExtraOpaqueData,
	knownRecords ...tlv.RecordProducer) (CustomRecords,
	fn.Set[tlv.Type], ExtraOpaqueData, error) {

	extraDataTlvMap, err := allExtraData.ExtractRecords(knownRecords...)
	if err != nil {
		return nil, nil, nil, err
	}

	// Remove the known and now extracted records from the leftover extra
	// data map.
	parsedKnownRecords := make(fn.Set[tlv.Type], len(knownRecords))
	for _, producer := range knownRecords {
		r := producer.Record()

		// Only remove the records if it was parsed (remainder is nil).
		// We'll just store the type so we can tell the caller which
		// records were actually parsed fully.
		val, ok := extraDataTlvMap[r.Type()]
		if ok && val == nil {
			parsedKnownRecords.Add(r.Type())
			delete(extraDataTlvMap, r.Type())
		}
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

	// Set the custom records field to the custom records specific TLV
	// record map.
	customRecords, err := NewCustomRecords(customRecordsTlvMap)
	if err != nil {
		return nil, nil, nil, err
	}

	// Encode the remaining records back into the extra data field. These
	// records are not in the custom records TLV type range and do not
	// have associated fields in the struct that produced the records.
	extraData, err := NewExtraOpaqueData(extraDataTlvMap)
	if err != nil {
		return nil, nil, nil, err
	}

	// Help with unit testing where we might have the empty value (nil) for
	// the extra data instead of the default that's returned by the
	// constructor (empty slice).
	if len(extraData) == 0 {
		extraData = nil
	}

	return customRecords, parsedKnownRecords, extraData, nil
}

// MergeAndEncode merges the known records with the extra data and custom
// records, then encodes the merged records into raw bytes.
func MergeAndEncode(knownRecords []tlv.RecordProducer,
	extraData ExtraOpaqueData, customRecords CustomRecords) ([]byte,
	error) {

	// Construct a slice of all the records that we should include in the
	// message extra data field. We will start by including any records from
	// the extra data field.
	mergedRecords, err := extraData.RecordProducers()
	if err != nil {
		return nil, err
	}

	// Merge the known and extra data records.
	mergedRecords = append(mergedRecords, knownRecords...)

	// Include custom records in the extra data wire field if they are
	// present. Ensure that the custom records are validated before encoding
	// them.
	if err := customRecords.Validate(); err != nil {
		return nil, fmt.Errorf("custom records validation error: %w",
			err)
	}

	// Extend the message extra data records slice with TLV records from the
	// custom records field.
	mergedRecords = append(
		mergedRecords, customRecords.RecordProducers()...,
	)

	// Now we can sort the records and make sure there are no records with
	// the same type that would collide when encoding.
	sortedRecords := ProduceRecordsSorted(mergedRecords...)
	if err := AssertUniqueTypes(sortedRecords); err != nil {
		return nil, err
	}

	return EncodeRecords(sortedRecords)
}

// ParseAndExtractExtraData parses the given extra data into the passed-in
// records, then returns any remaining records as extra data.
func ParseAndExtractExtraData(allTlvData ExtraOpaqueData,
	knownRecords ...tlv.RecordProducer) (fn.Set[tlv.Type],
	ExtraOpaqueData, error) {

	extraDataTlvMap, err := allTlvData.ExtractRecords(knownRecords...)
	if err != nil {
		return nil, nil, err
	}

	// Remove the known and now extracted records from the leftover extra
	// data map.
	parsedKnownRecords := make(fn.Set[tlv.Type], len(knownRecords))
	for _, producer := range knownRecords {
		r := producer.Record()

		// Only remove the records if it was parsed (remainder is nil).
		// We'll just store the type so we can tell the caller which
		// records were actually parsed fully.
		val, ok := extraDataTlvMap[r.Type()]
		if ok && val == nil {
			parsedKnownRecords.Add(r.Type())
			delete(extraDataTlvMap, r.Type())
		}
	}

	// Encode the remaining records back into the extra data field. These
	// records are not in the custom records TLV type range and do not
	// have associated fields in the struct that produced the records.
	extraData, err := NewExtraOpaqueData(extraDataTlvMap)
	if err != nil {
		return nil, nil, err
	}

	return parsedKnownRecords, extraData, nil
}
