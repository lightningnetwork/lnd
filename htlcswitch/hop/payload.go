package hop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
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

	// InsufficientViolation indicates that the provided type does
	// not satisfy constraints.
	InsufficientViolation
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

	case InsufficientViolation:
		return "insufficient"

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

	// encryptedData is a blob of data encrypted by the receiver for use
	// in blinded routes.
	encryptedData []byte

	// blindingPoint is an ephemeral pubkey for use in blinded routes.
	blindingPoint *btcec.PublicKey

	// metadata is additional data that is sent along with the payment to
	// the payee.
	metadata []byte

	// totalAmtMsat holds the info provided in total_amount_msat when
	// parsed from a TLV onion payload.
	totalAmtMsat lnwire.MilliSatoshi
}

// NewLegacyPayload builds a Payload from the amount, cltv, and next hop
// parameters provided by leegacy onion payloads.
func NewLegacyPayload(f *sphinx.HopData) *Payload {
	nextHop := binary.BigEndian.Uint64(f.NextAddress[:])

	return &Payload{
		FwdInfo: ForwardingInfo{
			NextHop:         lnwire.NewShortChanIDFromInt(nextHop),
			AmountToForward: lnwire.MilliSatoshi(f.ForwardAmount),
			OutgoingCTLV:    f.OutgoingCltv,
		},
		customRecords: make(record.CustomSet),
	}
}

// NewPayloadFromReader builds a new Hop from the passed io.Reader. The reader
// should correspond to the bytes encapsulated in a TLV onion payload. A
// blinding kit is passed in to help handle payloads that are part of a blinded
// route.
func NewPayloadFromReader(r io.Reader, blindingKit *BlindingKit) (
	*Payload, error) {

	var (
		cid           uint64
		amt           uint64
		totalAmtMsat  uint64
		cltv          uint32
		mpp           = &record.MPP{}
		amp           = &record.AMP{}
		encryptedData []byte
		blindingPoint *btcec.PublicKey
		metadata      []byte
	)

	tlvStream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		record.NewNextHopIDRecord(&cid),
		mpp.Record(),
		record.NewEncryptedDataRecord(&encryptedData),
		record.NewBlindingPointRecord(&blindingPoint),
		amp.Record(),
		record.NewMetadataRecord(&metadata),
		record.NewTotalAmtMsatBlinded(&totalAmtMsat),
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
	_, err = ValidateParsedPayloadTypes(
		parsedTypes, nextHop, blindingKit, blindingPoint,
	)
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

	// If no encrypted data was parsed, set the field on our resulting
	// payload to nil.
	if _, ok := parsedTypes[record.EncryptedDataOnionType]; !ok {
		encryptedData = nil
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
			NextHop:         nextHop,
			AmountToForward: lnwire.MilliSatoshi(amt),
			OutgoingCTLV:    cltv,
		},
		MPP:           mpp,
		AMP:           amp,
		metadata:      metadata,
		encryptedData: encryptedData,
		blindingPoint: blindingPoint,
		customRecords: customRecords,
		totalAmtMsat:  lnwire.MilliSatoshi(totalAmtMsat),
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
// requirements for this method are described in BOLT 04. If the payload is for
// a blinded route, it also returns the blinding point that should be used for
// further payload processing.
func ValidateParsedPayloadTypes(parsedTypes tlv.TypeMap,
	nextHop lnwire.ShortChannelID,
	blindingKit *BlindingKit,
	onionBlinding *btcec.PublicKey) (*btcec.PublicKey, error) {

	// If encrypted data is present in our payload, validate fields for
	// a blinded route - this validation is different to regular payload
	// validation because some fields are contained in encrypted data
	// instead of the onion TLVs.
	_, dataPresent := parsedTypes[record.EncryptedDataOnionType]
	if dataPresent {
		return validateBlindedRouteTypes(
			parsedTypes, blindingKit, onionBlinding,
		)
	}

	isFinalHop := nextHop == Exit

	_, hasAmt := parsedTypes[record.AmtOnionType]
	_, hasLockTime := parsedTypes[record.LockTimeOnionType]
	_, hasNextHop := parsedTypes[record.NextHopOnionType]
	_, hasMPP := parsedTypes[record.MPPOnionType]
	_, hasAMP := parsedTypes[record.AMPOnionType]

	switch {

	// All hops must include an amount to forward.
	case !hasAmt:
		return nil, ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// All hops must include a cltv expiry.
	case !hasLockTime:
		return nil, ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: OmittedViolation,
			FinalHop:  isFinalHop,
		}

	// The exit hop should omit the next hop id. If nextHop != Exit, the
	// sender must have included a record, so we don't need to test for its
	// inclusion at intermediate hops directly.
	case isFinalHop && hasNextHop:
		return nil, ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: IncludedViolation,
			FinalHop:  true,
		}

	// Intermediate nodes should never receive MPP fields.
	case !isFinalHop && hasMPP:
		return nil, ErrInvalidPayload{
			Type:      record.MPPOnionType,
			Violation: IncludedViolation,
			FinalHop:  isFinalHop,
		}

	// Intermediate nodes should never receive AMP fields.
	case !isFinalHop && hasAMP:
		return nil, ErrInvalidPayload{
			Type:      record.AMPOnionType,
			Violation: IncludedViolation,
			FinalHop:  isFinalHop,
		}
	}

	return nil, nil
}

// validateBlindedRouteTypes performs the validation required for payloads
// that are part of a blinded route, returning the blinding point that is in
// use for that payload.
func validateBlindedRouteTypes(parsedTypes tlv.TypeMap,
	blindingKit *BlindingKit,
	onionBlinding *btcec.PublicKey) (*btcec.PublicKey, error) {

	if blindingKit == nil {
		return nil, errors.New("blinding kit required")
	}

	var (
		blindingPoint        *btcec.PublicKey
		updateAddBlindingSet = blindingKit.BlindingPoint != nil
		onionBlindingSet     = onionBlinding != nil
	)

	switch {
	// We should have a blinding key either in update_add_htlc or in the
	// onion, but not both.
	case updateAddBlindingSet && onionBlindingSet:
		return nil, ErrInvalidPayload{
			Type:      record.BlindingPointOnionType,
			Violation: IncludedViolation,
			FinalHop:  false,
		}

	case updateAddBlindingSet:
		blindingPoint = blindingKit.BlindingPoint

	case onionBlindingSet:
		blindingPoint = onionBlinding
	}

	if _, ok := parsedTypes[record.EncryptedDataOnionType]; !ok {
		return nil, ErrInvalidPayload{
			Type:      record.EncryptedDataOnionType,
			Violation: RequiredViolation,
			FinalHop:  false,
		}
	}

	// We have restrictions on the types of TLVs that we allow in
	// intermediate and final hops. Set a map of allowed TLVs and run a
	// check for any other non-nil values in our parsed map.
	allowedTLVs := map[tlv.Type]struct{}{
		record.EncryptedDataOnionType: {},
		record.BlindingPointOnionType: {},
	}

	// The last hop is allowed some additional TLVs.
	if blindingKit.lastHop {
		allowedTLVs[record.AmtOnionType] = struct{}{}
		allowedTLVs[record.LockTimeOnionType] = struct{}{}
	}

	for tlvType := range parsedTypes {
		if _, ok := allowedTLVs[tlvType]; ok {
			continue
		}

		return nil, ErrInvalidPayload{
			Type:      tlvType,
			Violation: IncludedViolation,
			FinalHop:  false,
		}
	}

	return blindingPoint, nil
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

// EncryptedData returns the route blinding encrypted data parsed from the
// onion payload.
func (h *Payload) EncryptedData() []byte {
	return h.encryptedData
}

// BlindingPoint returns the route blinding point parsed from the onion payload.
func (h *Payload) BlindingPoint() *btcec.PublicKey {
	return h.blindingPoint
}

// Metadata returns the additional data that is sent along with the
// payment to the payee.
func (h *Payload) Metadata() []byte {
	return h.metadata
}

// TotalAmtMsat returns the total amount sent to the final hop, as set by the
// payee.
func (h *Payload) TotalAmtMsat() lnwire.MilliSatoshi {
	return h.totalAmtMsat
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

// validateBlindedRouteData performs the additional validation that is
// required for payments that rely on data provided in an encrypted blob to
// be forwarded. We enforce additional constraints here to prevent malicious
// parties from probing portions of the blinded route to "un-blind" them.
func validateBlindedRouteData(blindedData *record.BlindedRouteData,
	incomingAmount lnwire.MilliSatoshi, incomingTimelock uint32) error {

	if blindedData.Constraints != nil {
		maxCLTV := blindedData.Constraints.MaxCltvExpiry
		if maxCLTV != 0 && incomingTimelock > maxCLTV {
			return ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: InsufficientViolation,
			}
		}

		if incomingAmount < blindedData.Constraints.HtlcMinimumMsat {
			return ErrInvalidPayload{
				Type:      record.AmtOnionType,
				Violation: InsufficientViolation,
			}
		}
	}

	// Fail if we don't understand any features (even or odd), because we
	// expect the features to have been set from our announcement.
	if blindedData.Features.UnknownFeatures() {
		return ErrInvalidPayload{
			Type:      record.FeatureVectorType,
			Violation: InsufficientViolation,
		}
	}

	return nil
}
