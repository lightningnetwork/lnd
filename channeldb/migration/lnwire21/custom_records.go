package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// MinCustomRecordsTlvType is the minimum custom records TLV type as
	// defined in BOLT 01.
	MinCustomRecordsTlvType = 65536
)

// CustomRecords stores a set of custom key/value pairs. Map keys are TLV types
// which must be greater than or equal to MinCustomRecordsTlvType.
type CustomRecords map[uint64][]byte

// NewCustomRecords creates a new CustomRecords instance from a
// tlv.TypeMap.
func NewCustomRecords(tlvMap tlv.TypeMap) (CustomRecords, error) {
	// Make comparisons in unit tests easy by returning nil if the map is
	// empty.
	if len(tlvMap) == 0 {
		return nil, nil
	}

	customRecords := make(CustomRecords, len(tlvMap))
	for k, v := range tlvMap {
		customRecords[uint64(k)] = v
	}

	// Validate the custom records.
	err := customRecords.Validate()
	if err != nil {
		return nil, fmt.Errorf("custom records from tlv map "+
			"validation error: %w", err)
	}

	return customRecords, nil
}

// ParseCustomRecords creates a new CustomRecords instance from a tlv.Blob.
func ParseCustomRecords(b tlv.Blob) (CustomRecords, error) {
	return ParseCustomRecordsFrom(bytes.NewReader(b))
}

// ParseCustomRecordsFrom creates a new CustomRecords instance from a reader.
func ParseCustomRecordsFrom(r io.Reader) (CustomRecords, error) {
	typeMap, err := DecodeRecords(r)
	if err != nil {
		return nil, fmt.Errorf("error decoding HTLC record: %w", err)
	}

	return NewCustomRecords(typeMap)
}

// Validate checks that all custom records are in the custom type range.
func (c CustomRecords) Validate() error {
	if c == nil {
		return nil
	}

	for key := range c {
		if key < MinCustomRecordsTlvType {
			return fmt.Errorf("custom records entry with TLV "+
				"type below min: %d", MinCustomRecordsTlvType)
		}
	}

	return nil
}

// Copy returns a copy of the custom records.
func (c CustomRecords) Copy() CustomRecords {
	if c == nil {
		return nil
	}

	customRecords := make(CustomRecords, len(c))
	for k, v := range c {
		customRecords[k] = v
	}

	return customRecords
}

// ExtendRecordProducers extends the given records slice with the custom
// records. The resultant records slice will be sorted if the given records
// slice contains TLV types greater than or equal to MinCustomRecordsTlvType.
func (c CustomRecords) ExtendRecordProducers(
	producers []tlv.RecordProducer) ([]tlv.RecordProducer, error) {

	// If the custom records are nil or empty, there is nothing to do.
	if len(c) == 0 {
		return producers, nil
	}

	// Validate the custom records.
	err := c.Validate()
	if err != nil {
		return nil, err
	}

	// Ensure that the existing records slice TLV types are not also present
	// in the custom records. If they are, the resultant extended records
	// slice would erroneously contain duplicate TLV types.
	for _, rp := range producers {
		record := rp.Record()
		recordTlvType := uint64(record.Type())

		_, foundDuplicateTlvType := c[recordTlvType]
		if foundDuplicateTlvType {
			return nil, fmt.Errorf("custom records contains a TLV "+
				"type that is already present in the "+
				"existing records: %d", recordTlvType)
		}
	}

	// Convert the custom records map to a TLV record producer slice and
	// append them to the exiting records slice.
	customRecordProducers := RecordsAsProducers(tlv.MapToRecords(c))
	producers = append(producers, customRecordProducers...)

	// If the records slice which was given as an argument included TLV
	// values greater than or equal to the minimum custom records TLV type
	// we will sort the extended records slice to ensure that it is ordered
	// correctly.
	SortProducers(producers)

	return producers, nil
}

// RecordProducers returns a slice of record producers for the custom records.
func (c CustomRecords) RecordProducers() []tlv.RecordProducer {
	// If the custom records are nil or empty, return an empty slice.
	if len(c) == 0 {
		return nil
	}

	// Convert the custom records map to a TLV record producer slice.
	records := tlv.MapToRecords(c)

	return RecordsAsProducers(records)
}

// Serialize serializes the custom records into a byte slice.
func (c CustomRecords) Serialize() ([]byte, error) {
	records := tlv.MapToRecords(c)
	return EncodeRecords(records)
}

// SerializeTo serializes the custom records into the given writer.
func (c CustomRecords) SerializeTo(w io.Writer) error {
	records := tlv.MapToRecords(c)
	return EncodeRecordsTo(w, records)
}

// ProduceRecordsSorted converts a slice of record producers into a slice of
// records and then sorts it by type.
func ProduceRecordsSorted(recordProducers ...tlv.RecordProducer) []tlv.Record {
	records := fn.Map(
		recordProducers,
		func(producer tlv.RecordProducer) tlv.Record {
			return producer.Record()
		},
	)

	// Ensure that the set of records are sorted before we attempt to
	// decode from the stream, to ensure they're canonical.
	tlv.SortRecords(records)

	return records
}

// SortProducers sorts the given record producers by their type.
func SortProducers(producers []tlv.RecordProducer) {
	sort.Slice(producers, func(i, j int) bool {
		recordI := producers[i].Record()
		recordJ := producers[j].Record()
		return recordI.Type() < recordJ.Type()
	})
}

// TlvMapToRecords converts a TLV map into a slice of records.
func TlvMapToRecords(tlvMap tlv.TypeMap) []tlv.Record {
	tlvMapGeneric := make(map[uint64][]byte)
	for k, v := range tlvMap {
		tlvMapGeneric[uint64(k)] = v
	}

	return tlv.MapToRecords(tlvMapGeneric)
}

// RecordsAsProducers converts a slice of records into a slice of record
// producers.
func RecordsAsProducers(records []tlv.Record) []tlv.RecordProducer {
	return fn.Map(records, func(record tlv.Record) tlv.RecordProducer {
		return &record
	})
}

// EncodeRecords encodes the given records into a byte slice.
func EncodeRecords(records []tlv.Record) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncodeRecordsTo(&buf, records); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// EncodeRecordsTo encodes the given records into the given writer.
func EncodeRecordsTo(w io.Writer, records []tlv.Record) error {
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// DecodeRecords decodes the given byte slice into the given records and returns
// the rest as a TLV type map.
func DecodeRecords(r io.Reader,
	records ...tlv.Record) (tlv.TypeMap, error) {

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	return tlvStream.DecodeWithParsedTypes(r)
}

// DecodeRecordsP2P decodes the given byte slice into the given records and
// returns the rest as a TLV type map. This function is identical to
// DecodeRecords except that the record size is capped at 65535.
func DecodeRecordsP2P(r *bytes.Reader,
	records ...tlv.Record) (tlv.TypeMap, error) {

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	return tlvStream.DecodeWithParsedTypesP2P(r)
}

// AssertUniqueTypes asserts that the given records have unique types.
func AssertUniqueTypes(r []tlv.Record) error {
	seen := make(fn.Set[tlv.Type], len(r))
	for _, record := range r {
		t := record.Type()
		if seen.Contains(t) {
			return fmt.Errorf("duplicate record type: %d", t)
		}
		seen.Add(t)
	}

	return nil
}
