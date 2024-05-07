package lnwire

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// MinCustomRecordsTlvType is the minimum custom records TLV type as
	// defined in BOLT 01.
	MinCustomRecordsTlvType = 65536
)

// ParseCustomRecords decodes a set of custom records from a byte slice.
func ParseCustomRecords(b tlv.Blob) (CustomRecords, error) {
	stream, err := tlv.NewStream()
	if err != nil {
		return nil, fmt.Errorf("error creating stream: %w", err)
	}

	typeMap, err := stream.DecodeWithParsedTypes(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("error decoding HTLC record: %w", err)
	}

	customRecords := make(CustomRecords, len(typeMap))
	for k, v := range typeMap {
		customRecords[uint64(k)] = v
	}

	return customRecords, nil
}

// CustomRecords stores a set of custom key/value pairs. Map keys are TLV types
// which must be greater than or equal to MinCustomRecordsTlvType.
type CustomRecords map[uint64][]byte

// NewCustomRecordsFromTlvTypeMap creates a new CustomRecords instance from a
// tlv.TypeMap.
func NewCustomRecordsFromTlvTypeMap(tlvMap tlv.TypeMap) (CustomRecords,
	error) {

	customRecords := make(CustomRecords, len(tlvMap))
	for k, v := range tlvMap {
		customRecords[uint64(k)] = v
	}

	// Validate the custom records.
	err := customRecords.Validate()
	if err != nil {
		return nil, fmt.Errorf("custom records from tlv map "+
			"validation error: %v", err)
	}

	return customRecords, nil
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

func (c CustomRecords) Copy() CustomRecords {
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
	if c == nil || len(c) == 0 {
		return producers, nil
	}

	// Validate the custom records.
	err := c.Validate()
	if err != nil {
		return nil, err
	}

	// Keep track of the maximum record TLV type in the given records
	// slice. This will be used to determine whether the extended records
	// slice requires sorting.
	maxRecordTlvType := uint64(0)

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

		// Keep track of the maximum record TLV type.
		if recordTlvType > maxRecordTlvType {
			maxRecordTlvType = recordTlvType
		}
	}

	// Convert the custom records map to a TLV record producer slice and
	// append them to the exiting records slice.
	crRecords := tlv.MapToRecords(c)
	for _, record := range crRecords {
		r := recordProducer{record}
		producers = append(producers, &r)
	}

	// If the records slice which was given as an argument included TLV
	// values greater than or equal to the minimum custom records TLV type
	// we will sort the extended records slice to ensure that it is ordered
	// correctly.
	sort.Slice(producers, func(i, j int) bool {
		recordI := producers[i].Record()
		recordJ := producers[j].Record()
		return recordI.Type() < recordJ.Type()
	})

	return producers, nil
}

func (c CustomRecords) Serialize() ([]byte, error) {
	records := tlv.MapToRecords(c)
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("error creating stream: %w", err)
	}

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return nil, fmt.Errorf("error encoding custom records: %w", err)
	}

	return b.Bytes(), nil
}
