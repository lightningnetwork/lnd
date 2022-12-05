package hop

import (
	"encoding/binary"
	"fmt"
	"io"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// PayloadViolation is an enum encapsulating the possible invalid payload
// violations that can occur when processing or validating a payload.
type PayloadViolation byte

const (
	// OmittedViolation indicates that a type was expected to be found the
	// payload but was absent.
	OmittedViolation PayloadViolation = iota

	// IncludedViolation indicates that a type was expected to be omitted
	// from the payload but was present.
	IncludedViolation

	// RequiredViolation indicates that an unknown even type was found in
	// the payload that we could not process.
	RequiredViolation
)

// String returns a human-readable description of the violation as a verb.
func (v PayloadViolation) String() string {
	switch v {
	case OmittedViolation:
		return "omitted"

	case IncludedViolation:
		return "included"

	case RequiredViolation:
		return "required"

	default:
		return "unknown violation"
	}
}

// ErrInvalidPayload is an error returned when a parsed onion payload either
// included or omitted incorrect records for a particular hop type.
type ErrInvalidPayload struct {
	// Type the record's type that cause the violation.
	Type tlv.Type

	// Violation is an enum indicating the type of violation detected in
	// processing Type.
	Violation PayloadViolation

	// FinalHop if true, indicates that the violation is for the final hop
	// in the route (identified by next hop id), otherwise the violation is
	// for an intermediate hop.
	FinalHop bool
}

// Error returns a human-readable description of the invalid payload error.
func (e ErrInvalidPayload) Error() string {
	hopType := "intermediate"
	if e.FinalHop {
		hopType = "final"
	}

	return fmt.Sprintf("onion payload for %s hop %v record with type %d",
		hopType, e.Violation, e.Type)
}

// Payload encapsulates all information delivered to a hop in an onion payload.
// A Hop can represent either a TLV or legacy payload. The primary forwarding
// instruction can be accessed via ForwardingInfo, and additional records can be
// accessed by other member functions.
type Payload struct {
	// FwdInfo holds the basic parameters required for HTLC forwarding, e.g.
	// amount, cltv, and next hop.
	FwdInfo ForwardingInfo

	// MPP holds the info provided in an option_mpp record when parsed from
	// a TLV onion payload.
	MPP *record.MPP

	// AMP holds the info provided in an option_amp record when parsed from
	// a TLV onion payload.
	AMP *record.AMP

	// customRecords are user-defined records in the custom type range that
	// were included in the payload.
	customRecords record.CustomSet

	// metadata is additional data that is sent along with the payment to
	// the payee.
	metadata []byte
}

// NewLegacyPayload builds a Payload from the amount, cltv, and next hop
// parameters provided by leegacy onion payloads.
func NewLegacyPayload(f *sphinx.HopData) *Payload {
	nextHop := binary.BigEndian.Uint64(f.NextAddress[:])

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         BitcoinNetwork,
			NextHop:         lnwire.NewShortChanIDFromInt(nextHop),
			AmountToForward: lnwire.MilliSatoshi(f.ForwardAmount),
			OutgoingCTLV:    f.OutgoingCltv,
		},
		customRecords: make(record.CustomSet),
	}
}

// NewPayloadFromReader builds a new Hop from the passed io.Reader. The reader
// should correspond to the bytes encapsulated in a TLV onion payload.
func NewPayloadFromReader(r io.Reader) (*Payload, error) {
	var (
		cid      uint64
		amt      uint64
		cltv     uint32
		mpp      = &record.MPP{}
		amp      = &record.AMP{}
		metadata []byte
	)

	tlvStream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		record.NewNextHopIDRecord(&cid),
		mpp.Record(),
		amp.Record(),
		record.NewMetadataRecord(&metadata),
	)
	if err != nil {
		return nil, err
	}

	// Since this data is provided by a potentially malicious peer, pass it
	// into the P2P decoding variant.
	parsedTypes, err := tlvStream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return nil, err
	}

	// Validate whether the sender properly included or omitted tlv records
	// in accordance with BOLT 04.
	nextHop := lnwire.NewShortChanIDFromInt(cid)
	err = ValidateParsedPayloadTypes(parsedTypes, nextHop)
	if err != nil {
		return nil, err
	}

	// Check for violation of the rules for mandatory fields.
	violatingType := getMinRequiredViolation(parsedTypes)
	if violatingType != nil {
		return nil, ErrInvalidPayload{
			Type:      *violatingType,
			Violation: RequiredViolation,
			FinalHop:  nextHop == Exit,
		}
	}

	// If no MPP field was parsed, set the MPP field on the resulting
	// payload to nil.
	if _, ok := parsedTypes[record.MPPOnionType]; !ok {
		mpp = nil
	}

	// If no AMP field was parsed, set the MPP field on the resulting
	// payload to nil.
	if _, ok := parsedTypes[record.AMPOnionType]; !ok {
		amp = nil
	}

	// If no metadata field was parsed, set the metadata field on the
	// resulting payload to nil.
	if _, ok := parsedTypes[record.MetadataOnionType]; !ok {
		metadata = nil
	}

	// Filter out the custom records.
	customRecords := NewCustomRecords(parsedTypes)

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         BitcoinNetwork,
			NextHop:         nextHop,
			AmountToForward: lnwire.MilliSatoshi(amt),
			OutgoingCTLV:    cltv,
		},
		MPP:           mpp,
		AMP:           amp,
		metadata:      metadata,
		customRecords: customRecords,
	}, nil
}

// ForwardingInfo returns the basic parameters required for HTLC forwarding,
// e.g. amount, cltv, and next hop.
func (h *Payload) ForwardingInfo() ForwardingInfo {
	return h.FwdInfo
}

// NewCustomRecords filters the types parsed from the tlv stream for custom
// records.
func NewCustomRecords(parsedTypes tlv.TypeMap) record.CustomSet {
	customRecords := make(record.CustomSet)
	for t, parseResult := range parsedTypes {
		if parseResult == nil || t < record.CustomTypeStart {
			continue
		}
		customRecords[uint64(t)] = parseResult
	}
	return customRecords
}

// ValidateParsedPayloadTypes checks the types parsed from a hop payload to
// ensure that the proper fields are either included or omitted. The finalHop
// boolean should be true if the payload was parsed for an exit hop. The
// requirements for this method are described in BOLT 04.
func ValidateParsedPayloadTypes(parsedTypes tlv.TypeMap,
	nextHop lnwire.ShortChannelID) error {

	isFinalHop := nextHop == Exit

	_, hasAmt := parsedTypes[record.AmtOnionType]
	_, hasLockTime := parsedTypes[record.LockTimeOnionType]
	_, hasNextHop := parsedTypes[record.NextHopOnionType]
	_, hasMPP := parsedTypes[record.MPPOnionType]
	_, hasAMP := parsedTypes[record.AMPOnionType]

	switch {

	// All hops must include an amount to forward.
	case !hasAmt:
		return ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// All hops must include a cltv expiry.
	case !hasLockTime:
		return ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// The exit hop should omit the next hop id. If nextHop != Exit, the
	// sender must have included a record, so we don't need to test for its
	// inclusion at intermediate hops directly.
	case isFinalHop && hasNextHop:
		return ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: IncludedViolation,
			FinalHop:  true,
		}

	// Intermediate nodes should never receive MPP fields.
	case !isFinalHop && hasMPP:
		return ErrInvalidPayload{
			Type:      record.MPPOnionType,
			Violation: IncludedViolation,
			FinalHop:  isFinalHop,
		}

	// Intermediate nodes should never receive AMP fields.
	case !isFinalHop && hasAMP:
		return ErrInvalidPayload{
			Type:      record.AMPOnionType,
			Violation: IncludedViolation,
			FinalHop:  isFinalHop,
		}
	}

	return nil
}

// MultiPath returns the record corresponding the option_mpp parsed from the
// onion payload.
func (h *Payload) MultiPath() *record.MPP {
	return h.MPP
}

// AMPRecord returns the record corresponding with option_amp parsed from the
// onion payload.
func (h *Payload) AMPRecord() *record.AMP {
	return h.AMP
}

// CustomRecords returns the custom tlv type records that were parsed from the
// payload.
func (h *Payload) CustomRecords() record.CustomSet {
	return h.customRecords
}

// Metadata returns the additional data that is sent along with the
// payment to the payee.
func (h *Payload) Metadata() []byte {
	return h.metadata
}

// getMinRequiredViolation checks for unrecognized required (even) fields in the
// standard range and returns the lowest required type. Always returning the
// lowest required type allows a failure message to be deterministic.
func getMinRequiredViolation(set tlv.TypeMap) *tlv.Type {
	var (
		requiredViolation        bool
		minRequiredViolationType tlv.Type
	)
	for t, parseResult := range set {
		// If a type is even but not known to us, we cannot process the
		// payload. We are required to understand a field that we don't
		// support.
		//
		// We always accept custom fields, because a higher level
		// application may understand them.
		if parseResult == nil || t%2 != 0 ||
			t >= record.CustomTypeStart {

			continue
		}

		if !requiredViolation || t < minRequiredViolationType {
			minRequiredViolationType = t
		}
		requiredViolation = true
	}

	if requiredViolation {
		return &minRequiredViolationType
	}

	return nil
}
