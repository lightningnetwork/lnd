package lnwire

// For some reason golangci-lint has a false positive on the sort order of the
// imports for the new "maps" package... We need the nolint directive here to
// ignore that.
//
//nolint:gci
import (
	"bytes"
	"fmt"
	"io"
	"maps"

	"github.com/lightningnetwork/lnd/tlv"
)

// ExtraOpaqueData is the set of data that was appended to this message, some
// of which we may not actually know how to iterate or parse. By holding onto
// this data, we ensure that we're able to properly validate the set of
// signatures that cover these new fields, and ensure we're able to make
// upgrades to the network in a forwards compatible manner.
type ExtraOpaqueData []byte

// NewExtraOpaqueDataFromTlvTypeMap creates a new ExtraOpaqueData instance from
// a tlv.TypeMap.
func NewExtraOpaqueDataFromTlvTypeMap(tlvMap tlv.TypeMap) (ExtraOpaqueData,
	error) {

	extraData := ExtraOpaqueData{}

	// If the tlv map is empty, return an empty extra data instance.
	if len(tlvMap) == 0 {
		return extraData, nil
	}

	// Convert the tlv map to a generic type map.
	tlvMapGeneric := make(map[uint64][]byte)
	for k, v := range tlvMap {
		tlvMapGeneric[uint64(k)] = v
	}

	// Convert the generic type map to a slice of records.
	records := tlv.MapToRecords(tlvMapGeneric)

	// Encode the records into the extra data byte slice.
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var bytesWriter bytes.Buffer
	if err := tlvStream.Encode(&bytesWriter); err != nil {
		return nil, err
	}

	return bytesWriter.Bytes(), nil
}

// Encode attempts to encode the raw extra bytes into the passed io.Writer.
func (e *ExtraOpaqueData) Encode(w *bytes.Buffer) error {
	eBytes := []byte((*e)[:])
	if err := WriteBytes(w, eBytes); err != nil {
		return err
	}

	return nil
}

// Decode attempts to unpack the raw bytes encoded in the passed io.Reader as a
// set of extra opaque data.
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

	// Ensure that the set of records are sorted before we attempt to
	// decode from the stream, to ensure they're canonical.
	tlv.SortRecords(records)

	extraBytesReader := bytes.NewReader(*e)

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	// Since ExtraOpaqueData is provided by a potentially malicious peer,
	// pass it into the P2P decoding variant.
	return tlvStream.DecodeWithParsedTypesP2P(extraBytesReader)
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

	// Convert the TLV map into a slice of records.
	tlvMapGeneric := make(map[uint64][]byte)
	for k, v := range tlvMap {
		tlvMapGeneric[uint64(k)] = v
	}
	records := tlv.MapToRecords(tlvMapGeneric)

	// Convert the records to record producers.
	recordProducers = make([]tlv.RecordProducer, len(records))
	for i, record := range records {
		recordProducers[i] = &recordProducer{record}
	}

	return recordProducers, nil
}

// Copy returns a copy of the target ExtraOpaqueData instance.
func (e *ExtraOpaqueData) Copy() ExtraOpaqueData {
	copyData := make([]byte, len(*e))
	copy(copyData, *e)
	return copyData
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

// wireTlvMap is a struct that holds the official records and custom records in
// a TLV type map. This is useful for ensuring that the set of custom TLV
// records are handled properly and don't overlap with the official records.
type wireTlvMap struct {
	// officialTypes is the set of official records that are defined in the
	// spec.
	officialTypes tlv.TypeMap

	// customTypes is the set of custom records that are not defined in
	// spec, and are used by higher level applications.
	customTypes tlv.TypeMap
}

// newWireTlvMap creates a new tlv.TypeMap from the given set of parsed TLV
// records. A struct with two maps are returned:
//
//  1. officialTypes: the set of official records that are defined in the
//     spec.
//
//  2. customTypes: the set of custom records that are not defined in
//     the spec.
func newWireTlvMap(typeMap tlv.TypeMap) wireTlvMap {
	officialRecords := maps.Clone(typeMap)

	// Any records from the extra data TLV map which are in the custom
	// records TLV type range will be included in the custom records field
	// and removed from the extra data field.
	customRecordsTlvMap := make(tlv.TypeMap, len(typeMap))
	for k, v := range typeMap {
		// Skip records that are not in the custom records TLV type
		// range.
		if k < MinCustomRecordsTlvType {
			continue
		}

		// Include the record in the custom records map.
		customRecordsTlvMap[k] = v

		// Now that the record is included in the custom records map,
		// we can remove it from the extra data TLV map.
		delete(officialRecords, k)
	}

	return wireTlvMap{
		officialTypes: officialRecords,
		customTypes:   customRecordsTlvMap,
	}
}

// Len returns the total number of records in the wireTlvMap.
func (w *wireTlvMap) Len() int {
	return len(w.officialTypes) + len(w.customTypes)
}
