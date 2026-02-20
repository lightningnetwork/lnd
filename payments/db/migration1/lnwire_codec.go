package migration1

// lnwire_codec.go contains frozen, self-contained copies of lnwire
// serialization functions used by the migration1 KV store. These local copies
// ensure that the serialization boundary of this package is independent of
// live lnwire package changes that may occur after this migration is released.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// parseCustomRecordsFrom creates a new CustomRecords instance from a reader
// by decoding all TLV records and validating that each key is in the custom
// records range.
//
// This is a frozen local copy of lnwire.ParseCustomRecordsFrom.
func parseCustomRecordsFrom(r io.Reader) (lnwire.CustomRecords, error) {
	tlvStream, err := tlv.NewStream()
	if err != nil {
		return nil, err
	}

	typeMap, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, fmt.Errorf("error decoding HTLC record: %w", err)
	}

	if len(typeMap) == 0 {
		return nil, nil
	}

	customRecords := make(lnwire.CustomRecords, len(typeMap))
	for k, v := range typeMap {
		customRecords[uint64(k)] = v
	}

	for key := range customRecords {
		if key < lnwire.MinCustomRecordsTlvType {
			return nil, fmt.Errorf("custom records entry with "+
				"TLV type below min: %d", key)
		}
	}

	return customRecords, nil
}

// mergeAndEncode merges the known records with the custom records, sorts them,
// validates uniqueness, and encodes the result to a byte slice.
//
// This is a frozen local copy of lnwire.MergeAndEncode. Note that the
// extraData parameter is kept for API compatibility but its content is encoded
// before being merged with the other records.
func mergeAndEncode(knownRecords []tlv.RecordProducer,
	extraData []byte, customRecords lnwire.CustomRecords) ([]byte, error) {

	// Start by parsing any records from the extra opaque data blob.
	var merged []tlv.RecordProducer
	if len(extraData) > 0 {
		extraStream, err := tlv.NewStream()
		if err != nil {
			return nil, err
		}

		extraMap, err := extraStream.DecodeWithParsedTypes(
			bytes.NewReader(extraData),
		)
		if err != nil {
			return nil, fmt.Errorf("error decoding extra "+
				"data: %w", err)
		}

		// Convert the extra data type map to record producers.
		extraGeneric := make(map[uint64][]byte, len(extraMap))
		for t, v := range extraMap {
			extraGeneric[uint64(t)] = v
		}
		extraSlice := tlv.MapToRecords(extraGeneric)
		for i := range extraSlice {
			merged = append(merged, &extraSlice[i])
		}
	}

	merged = append(merged, knownRecords...)

	// Validate and add custom records.
	for key := range customRecords {
		if key < lnwire.MinCustomRecordsTlvType {
			return nil, fmt.Errorf("custom records validation "+
				"error: entry with TLV type below min: %d",
				key)
		}
	}

	customSlice := tlv.MapToRecords(map[uint64][]byte(customRecords))
	for i := range customSlice {
		merged = append(merged, &customSlice[i])
	}

	// Sort producers by record type.
	sort.Slice(merged, func(i, j int) bool {
		ri, rj := merged[i].Record(), merged[j].Record()
		return ri.Type() < rj.Type()
	})

	// Materialise sorted records and assert uniqueness.
	records := make([]tlv.Record, 0, len(merged))
	seen := make(map[tlv.Type]bool, len(merged))
	for _, p := range merged {
		r := p.Record()
		if seen[r.Type()] {
			return nil, fmt.Errorf("duplicate record type: %d",
				r.Type())
		}
		seen[r.Type()] = true
		records = append(records, r)
	}

	// Encode the merged records.
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// parseAndExtractCustomRecords parses the given extra data blob as a TLV
// stream, extracts the provided known records into their respective fields,
// and returns the remaining records that fall within the custom records TLV
// type range.
//
// This is a frozen local copy of lnwire.ParseAndExtractCustomRecords, trimmed
// to only return the fields this package actually uses.
func parseAndExtractCustomRecords(allExtraData []byte,
	knownRecords ...tlv.RecordProducer) (lnwire.CustomRecords, error) {

	// Produce sorted records for the known fields so the TLV stream can
	// decode them in order.
	sortedKnown := make([]tlv.Record, len(knownRecords))
	for i, p := range knownRecords {
		sortedKnown[i] = p.Record()
	}
	sort.Slice(sortedKnown, func(i, j int) bool {
		return sortedKnown[i].Type() < sortedKnown[j].Type()
	})

	stream, err := tlv.NewStream(sortedKnown...)
	if err != nil {
		return nil, err
	}

	typeMap, err := stream.DecodeWithParsedTypes(
		bytes.NewReader(allExtraData),
	)
	if err != nil {
		return nil, err
	}

	// Remove successfully parsed known records from the remaining type map.
	for _, producer := range knownRecords {
		r := producer.Record()
		if val, ok := typeMap[r.Type()]; ok && val == nil {
			delete(typeMap, r.Type())
		}
	}

	// Collect remaining records that are in the custom records TLV range.
	customRecordsTlvMap := make(map[uint64][]byte)
	for k, v := range typeMap {
		if uint64(k) < lnwire.MinCustomRecordsTlvType {
			continue
		}
		customRecordsTlvMap[uint64(k)] = v
	}

	if len(customRecordsTlvMap) == 0 {
		return nil, nil
	}

	return lnwire.CustomRecords(customRecordsTlvMap), nil
}

// errParsingExtraTLVBytes is returned when extra bytes at the end of a failure
// message cannot be parsed as a valid TLV stream.
//
// This is a frozen local copy of lnwire.ErrParsingExtraTLVBytes.
var errParsingExtraTLVBytes = fmt.Errorf("error parsing extra TLV bytes")

// encodeFailureMessage encodes just the failure message without adding a
// length and padding the message for the onion protocol.
//
// This is a frozen local copy of lnwire.EncodeFailureMessage.
func encodeFailureMessage(w *bytes.Buffer,
	failure lnwire.FailureMessage, pver uint32) error {

	// First, we'll write out the error code itself into the failure
	// buffer.
	var codeBytes [2]byte
	code := uint16(failure.Code())
	binary.BigEndian.PutUint16(codeBytes[:], code)
	if _, err := w.Write(codeBytes[:]); err != nil {
		return err
	}

	// Next, some messages have an additional payload; if this is one of
	// those types, encode it as well.
	if f, ok := failure.(lnwire.Serializable); ok {
		if err := f.Encode(w, pver); err != nil {
			return err
		}
	}

	return nil
}

// makeEmptyOnionError returns an empty (zero-value) concrete FailureMessage
// matching the given failure code, ready to be populated by Decode.
//
// This is a frozen local copy of the unexported lnwire.makeEmptyOnionError.
func makeEmptyOnionError(code lnwire.FailCode) (lnwire.FailureMessage, error) {
	switch code {
	case lnwire.CodeInvalidRealm:
		return &lnwire.FailInvalidRealm{}, nil

	case lnwire.CodeTemporaryNodeFailure:
		return &lnwire.FailTemporaryNodeFailure{}, nil

	case lnwire.CodePermanentNodeFailure:
		return &lnwire.FailPermanentNodeFailure{}, nil

	case lnwire.CodeRequiredNodeFeatureMissing:
		return &lnwire.FailRequiredNodeFeatureMissing{}, nil

	case lnwire.CodePermanentChannelFailure:
		return &lnwire.FailPermanentChannelFailure{}, nil

	case lnwire.CodeRequiredChannelFeatureMissing:
		return &lnwire.FailRequiredChannelFeatureMissing{}, nil

	case lnwire.CodeUnknownNextPeer:
		return &lnwire.FailUnknownNextPeer{}, nil

	case lnwire.CodeIncorrectOrUnknownPaymentDetails:
		return &lnwire.FailIncorrectDetails{}, nil

	case lnwire.CodeIncorrectPaymentAmount:
		return &lnwire.FailIncorrectPaymentAmount{}, nil

	case lnwire.CodeFinalExpiryTooSoon:
		return &lnwire.FailFinalExpiryTooSoon{}, nil

	case lnwire.CodeInvalidOnionVersion:
		return &lnwire.FailInvalidOnionVersion{}, nil

	case lnwire.CodeInvalidOnionHmac:
		return &lnwire.FailInvalidOnionHmac{}, nil

	case lnwire.CodeInvalidOnionKey:
		return &lnwire.FailInvalidOnionKey{}, nil

	case lnwire.CodeTemporaryChannelFailure:
		return &lnwire.FailTemporaryChannelFailure{}, nil

	case lnwire.CodeAmountBelowMinimum:
		return &lnwire.FailAmountBelowMinimum{}, nil

	case lnwire.CodeFeeInsufficient:
		return &lnwire.FailFeeInsufficient{}, nil

	case lnwire.CodeIncorrectCltvExpiry:
		return &lnwire.FailIncorrectCltvExpiry{}, nil

	case lnwire.CodeExpiryTooSoon:
		return &lnwire.FailExpiryTooSoon{}, nil

	case lnwire.CodeChannelDisabled:
		return &lnwire.FailChannelDisabled{}, nil

	case lnwire.CodeFinalIncorrectCltvExpiry:
		return &lnwire.FailFinalIncorrectCltvExpiry{}, nil

	case lnwire.CodeFinalIncorrectHtlcAmount:
		return &lnwire.FailFinalIncorrectHtlcAmount{}, nil

	case lnwire.CodeExpiryTooFar:
		return &lnwire.FailExpiryTooFar{}, nil

	case lnwire.CodeInvalidOnionPayload:
		return &lnwire.InvalidOnionPayload{}, nil

	case lnwire.CodeMPPTimeout:
		return &lnwire.FailMPPTimeout{}, nil

	case lnwire.CodeInvalidBlinding:
		return &lnwire.FailInvalidBlinding{}, nil

	default:
		return nil, fmt.Errorf("unknown error code: %v", code)
	}
}

// decodeFailureMessage decodes just the failure message, ignoring any padding
// that may be present at the end.
//
// This is a frozen local copy of lnwire.DecodeFailureMessage.
func decodeFailureMessage(r io.Reader,
	pver uint32) (lnwire.FailureMessage, error) {

	// Read the two-byte failure code.
	var codeBytes [2]byte
	if _, err := io.ReadFull(r, codeBytes[:]); err != nil {
		return nil, fmt.Errorf("unable to read failure code: %w", err)
	}
	failCode := lnwire.FailCode(binary.BigEndian.Uint16(codeBytes[:]))

	// Create an empty failure of the correct concrete type.
	failure, err := makeEmptyOnionError(failCode)
	if err != nil {
		return nil, fmt.Errorf("unable to make empty error: %w", err)
	}

	// If the failure type carries a payload, decode it now.
	if f, ok := failure.(lnwire.Serializable); ok {
		if err := f.Decode(r, pver); err != nil {
			return nil, fmt.Errorf("unable to decode error "+
				"update (type=%T): %w", failure, err)
		}
	}

	return failure, nil
}
