package hop

import (
	"encoding/binary"
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

	// OverloadedViolation indicates that an expected type was provided
	// in more than one place. (used only for blinding point)
	OverloadedViolation

	// InsufficientViolation indicates that the provided type does
	// not satisfy constraints.
	// NOTE(10/5/22): Used for payment constraints. Does this belong here?
	// Should payment constraints be handled separately?
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

	case OverloadedViolation:
		return "overloaded"

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

	// BlindHop indicates that the violation occurred while our node was
	// processing a hop for a blinded route, otherwise we are a hop in a normal route.
	BlindHop bool
}

// Error returns a human-readable description of the invalid payload error.
func (e ErrInvalidPayload) Error() string {
	hopType := "intermediate"
	if e.FinalHop {
		hopType = "final"
	}

	payloadType := "onion"
	if e.BlindHop {
		payloadType = "route blinding"
	}

	return fmt.Sprintf("%s payload for %s hop %v record with type %d",
		payloadType, hopType, e.Violation, e.Type)
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

	// RouteBlindingEncryptedData contains information needed to forward in
	// the blinded portion of a route.
	RouteBlindingEncryptedData []byte

	// BlindingPoint delivered to the introductory node in the blinded route.
	// NOTE: Could use [33]byte for compressed pubkey and remove btcec dependency.
	BlindingPoint *btcec.PublicKey

	// customRecords are user-defined records in the custom type range that
	// were included in the payload.
	customRecords record.CustomSet

	// metadata is additional data that is sent along with the payment to
	// the payee.
	metadata []byte

	// TotalAmountMsat is the total payment amount.
	TotalAmountMsat lnwire.MilliSatoshi
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

// NOTE(10/26/22): This function is currently only used to get around
// the fact that customRecords is unexported and is required to be set.
// I don't think a function like this would have much utility otherwise.
// Given that we use TLV en/decoding I am a bit unsure how this function
// will behave/get access to the information it would need.
// The data included in a TLV payload is highly variable. That is why it makes
// sense to define a struct and then TLV encode each of the structs types
// that the user sets.
func NewTLVPayload() *Payload {

	// Set the unexported customRecords field so that we can carry
	// on with our Link testing.
	return &Payload{
		FwdInfo:       ForwardingInfo{},
		customRecords: make(record.CustomSet),
	}
}

// BlindHopPayload encapsulates all the route blinding information
// which will be parsed by nodes in the blinded portion of a route.
type BlindHopPayload struct {
	// probably don't need to save this as it's only used to ensure
	// all payloads on a blinded route are the same length.
	Padding               []byte
	NextHop               lnwire.ShortChannelID
	NextNodeID            *btcec.PublicKey
	PathID                []byte
	BlindingPointOverride *btcec.PublicKey
	PaymentRelay          *record.PaymentRelay
	PaymentConstraints    *record.PaymentConstraints
	// AllowedFeatures    *record.AllowedFeatures
}

const (
	// feeRateParts is the total number of parts used to express fee rates.
	feeRateParts = 1000000
)

// ForwardingInfo returns the basic parameters required for HTLC forwarding
// in a blinded route, e.g. amount, cltv, and next hop.
func (b *BlindHopPayload) ForwardingInfo(incomingAmt lnwire.MilliSatoshi,
	incomingTimelock uint32) ForwardingInfo {

	return ForwardingInfo{
		Network: BitcoinNetwork,
		NextHop: b.NextHop, // Assumes the next channel ID is given.
		// TODO(11/15/22): Do not assume that the next hop will be
		// specified by short_channel_id. Add support for forwarding to
		// a node_id (convert node_id --> scid)
		// NextHop: computeNextHop(b),
		AmountToForward: computeAmountToForward(incomingAmt, b.PaymentRelay),
		OutgoingCTLV:    computeOutgoingCltv(incomingTimelock, b.PaymentRelay),
	}
}

// computeAmountToForward computes the amount to forward for HTLCs
// processed as part of a blinded route.
// QUESTION(10/17/22): Should there be something apart from the function
// signature to indicate these are to be used for blinded hops?
func computeAmountToForward(incomingAmt lnwire.MilliSatoshi,
	paymentRelay *record.PaymentRelay) lnwire.MilliSatoshi {

	var base, rate uint64
	if paymentRelay != nil {
		base = uint64(paymentRelay.BaseFee)
		rate = uint64(paymentRelay.FeeRate) //* 1000 // ppm
	}

	amt := uint64(incomingAmt)

	// NOTE(10/17/22): In normal routing, we are given the amount to
	// forward directly in the onion payload. We need only verify that
	// the difference between incoming and outgoing amounts satisfies
	// our fee requirement.
	//
	// When forwarding a payment, the fee we take is calculated, not on
	// the incoming amount, but rather on the amount we forward. We charge
	// fees based on our own liquidity we are forwarding downstream.
	// With route blinding, we are NOT given the amount to forward.
	// This unintuitive looking formula comes from the fact that without
	// the amount to forward, we cannot compute the fees taken directly.
	//
	// The amount to be forwarded can be computed as follows:
	//
	// 	amt_to_forward = incoming_amount - total_fees
	// 	total_fees = base_fee + amt_to_forward*(fee_rate/1000000)
	//
	// After substitution and some massaging you will get:
	//
	// 	amt_to_forward = (incoming_amount - base_fee) /
	//                       ( 1 + fee_rate / 1000000 )
	//
	// From there we use a ceiling formula for integer division so that
	// we always round up, otherwise the sender may receive slightly
	// less than intended:
	//
	// 	ceil(a/b) = (a + b - 1)/(b)
	//
	fwdAmount := ((amt-base)*feeRateParts + feeRateParts + rate - 1) /
		(feeRateParts + rate)
	// fwdAmount := divideCeil(
	// 	feeRateParts*(amt-base),
	// 	feeRateParts+rate,
	// )

	return lnwire.MilliSatoshi(fwdAmount)
}

// // divideCeil divides dividend by factor and rounds the result up.
// func divideCeil(dividend, factor uint64) uint64 {
// 	return (dividend + factor - 1) / factor
// }

// computeOutgoingCltv computes the outgoing timelock for HTLCs
// processed as part of a blinded route.
func computeOutgoingCltv(incomingTimelock uint32,
	paymentRelay *record.PaymentRelay) uint32 {

	if paymentRelay == nil {
		return incomingTimelock
	}

	return incomingTimelock - uint32(paymentRelay.CltvExpiryDelta)
}

// NewBlindHopPayloadFromReader parses a route blinding payload from
// the passed io.Reader. The reader should correspond to the bytes
// encapsulated in the encrypted route blinding payload after they
// have been decrypted.
func NewBlindHopPayloadFromReader(r io.Reader,
	isFinalHop bool) (*BlindHopPayload, error) {

	var (
		padding            []byte
		nextHop            uint64
		nextNodeID         *btcec.PublicKey
		pathID             []byte
		blindingOverride   *btcec.PublicKey
		paymentRelay       = &record.PaymentRelay{}
		paymentConstraints = &record.PaymentConstraints{}
	)

	tlvStream, err := tlv.NewStream(
		record.NewPaddingRecord(&padding),
		record.NewBlindedNextHopRecord(&nextHop),
		record.NewNextNodeIDRecord(&nextNodeID),
		record.NewPathIDRecord(&pathID),
		record.NewBlindingOverrideRecord(&blindingOverride),
		paymentRelay.Record(),
		paymentConstraints.Record(),
	)
	if err != nil {
		return nil, err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	// Validate whether the sender properly included or omitted
	// route blinding tlv records in accordance with BOLT 04 as
	// early as possible. Additional validation will be performed later.
	err = ValidateRouteBlindingPayloadTypes(parsedTypes, isFinalHop)
	if err != nil {
		return nil, err
	}

	violatingType := getMinRequiredViolation(parsedTypes)
	if violatingType != nil {
		return nil, ErrInvalidPayload{
			Type:      *violatingType,
			Violation: RequiredViolation,
			FinalHop:  isFinalHop,
		}
	}

	// NOTE(8/13/22): We set the fields on our struct representing the
	// route blinding payload to nil so later we can properly validate.
	// Reconcile this with above.
	//
	// If no padding field was parsed, set the padding field
	// on the resulting payload to nil.
	if _, ok := parsedTypes[record.PaddingOnionType]; !ok {
		padding = nil
	}

	// If no path ID field was parsed, set the path ID field
	// on the resulting payload to nil.
	if _, ok := parsedTypes[record.PathIDOnionType]; !ok {
		pathID = nil
	}

	// If no payment relay field was parsed, set the payment relay field
	// on the resulting payload to nil.
	if _, ok := parsedTypes[record.PaymentRelayOnionType]; !ok {
		paymentRelay = nil
	}

	// If no payment constraints field was parsed, set the payment
	// constraints field on the resulting payload to nil.
	if _, ok := parsedTypes[record.PaymentConstraintsOnionType]; !ok {
		paymentConstraints = nil
	}

	return &BlindHopPayload{
		// NOTE: Likely do not need to expose padding to callers.
		Padding:               padding,
		NextHop:               lnwire.NewShortChanIDFromInt(nextHop),
		NextNodeID:            nextNodeID,
		PathID:                pathID,
		BlindingPointOverride: blindingOverride,
		PaymentRelay:          paymentRelay,
		PaymentConstraints:    paymentConstraints,
	}, nil
}

// NewPayloadFromReader builds a new Hop from the passed io.Reader. The reader
// should correspond to the bytes encapsulated in a TLV onion payload.
func NewPayloadFromReader(r io.Reader, isFinalHop bool) (*Payload, error) {
	var (
		cid         uint64
		amt         uint64
		cltv        uint32
		mpp         = &record.MPP{}
		amp         = &record.AMP{}
		metadata    []byte
		totalAmount uint64

		blindedData   []byte
		blindingPoint *btcec.PublicKey
	)

	tlvStream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		record.NewNextHopIDRecord(&cid),
		mpp.Record(),
		record.NewRouteBlindingEncryptedDataRecord(&blindedData),
		record.NewBlindingPointRecord(&blindingPoint),
		amp.Record(),
		record.NewMetadataRecord(&metadata),
		record.NewTotalAmountMsatRecord(&totalAmount),
	)
	if err != nil {
		return nil, err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	// Validate whether the sender properly included or omitted tlv records
	// in accordance with BOLT 04.
	// NOTE(9/21/22): The 'nextHop' is passed so that our validation
	// function can make the determination as to whether we are the
	// final hop. We will replace this with all-zero onion HMAC.
	nextHop := lnwire.NewShortChanIDFromInt(cid)
	err = ValidateParsedPayloadTypes(parsedTypes, isFinalHop)
	if err != nil {
		return nil, err
	}

	// Check for violation of the rules for mandatory fields.
	violatingType := getMinRequiredViolation(parsedTypes)
	if violatingType != nil {
		return nil, ErrInvalidPayload{
			Type:      *violatingType,
			Violation: RequiredViolation,
			FinalHop:  isFinalHop,
		}
	}

	// If no MPP field was parsed, set the MPP field on the resulting
	// payload to nil.
	if _, ok := parsedTypes[record.MPPOnionType]; !ok {
		mpp = nil
	}

	// If no AMP field was parsed, set the AMP field on the resulting
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
		MPP: mpp,
		AMP: amp,
		// NOTE(9/2/22): This remains encrypted for now.
		// We will parse (and validate) again as TLV stream after decryption.
		RouteBlindingEncryptedData: blindedData,
		BlindingPoint:              blindingPoint,
		metadata:                   metadata,
		TotalAmountMsat:            lnwire.MilliSatoshi(totalAmount),
		customRecords:              customRecords,
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

// ValidateRouteBlindingPayloadTypes checks the types parsed from a route
// blinding payload to ensure that the proper fields are either included
// or omitted. The requirements for this method are described in BOLT 04.
func ValidateRouteBlindingPayloadTypes(parsedTypes tlv.TypeMap,
	isFinalHop bool) error {

	_, hasNextHop := parsedTypes[record.BlindedNextHopOnionType]
	_, hasNextNode := parsedTypes[record.NextNodeIDOnionType]
	_, hasPathID := parsedTypes[record.PathIDOnionType]
	_, hasForwardingParams := parsedTypes[record.PaymentRelayOnionType]

	// TODO(9/10/22): Figure out how to actually distinguish the final
	// hop in a blinded route as TLV payload reader.
	// UPDATE(9/15/22): Apparently this is supposed to be indicated by the
	// sphinx implementation.
	if !isFinalHop {
		// An intermediate hop MUST specify how the payment is to be forwarded.
		if !hasForwardingParams {
			return ErrInvalidPayload{
				Type:      record.PaymentRelayOnionType,
				Violation: OmittedViolation,
				FinalHop:  false,
				BlindHop:  true,
			}
		}

		// An intermedate hop MUST specify the node to which we should forward.
		if !hasNextHop && !hasNextNode {
			return ErrInvalidPayload{
				Type:      record.BlindedNextHopOnionType,
				Violation: OmittedViolation,
				FinalHop:  false,
				BlindHop:  true,
			}
		}
	} else {
		// The final hop MUST have a path_id with which we can validate
		// this payment is for a blind route we created.
		if !hasPathID {
			return ErrInvalidPayload{
				Type:      record.PathIDOnionType,
				Violation: OmittedViolation,
				FinalHop:  true,
				BlindHop:  true,
			}
		}
	}

	return nil
}

// ValidateParsedPayloadTypes checks the types parsed from a hop payload to
// ensure that the proper fields are either included or omitted. The finalHop
// boolean should be true if the payload was parsed for an exit hop. The
// requirements for this method are described in BOLT 04.
//
// NOTE(9/2/22): We now have validation to do across two levels of TLV
// payloads. What is present in one payload effects our expectation of what
// is present in the other payload. As a result, we will likely need to validate
// the payloads together, which means waiting until after the route blinding
// payload is decrypted. We would like to preserve the normal validation
// in the case we are forwarding for a normal (not blinded) route.
func ValidateParsedPayloadTypes(parsedTypes tlv.TypeMap,
	isFinalHop bool) error {

	// NOTE(9/15/22): This may not serve as a proper determination of
	// whether this is the final hop. Processing nodes in a blinded
	// route are permitted to have an empty next hop in the top level TLV
	// onion payload. They MUST have a next hop in the recipient encrypted
	// data payload however.
	// UPDATE(9/15/22): According to BOLT-04 this is supposed to be
	// indicated by the sphinx implementation when it encounters
	// an all-zero onion HMAC.
	// isFinalHop := nextHop == Exit

	_, hasAmt := parsedTypes[record.AmtOnionType]
	_, hasLockTime := parsedTypes[record.LockTimeOnionType]
	_, hasNextHop := parsedTypes[record.NextHopOnionType]
	_, hasMPP := parsedTypes[record.MPPOnionType]
	_, hasAMP := parsedTypes[record.AMPOnionType]
	_, isBlindHop := parsedTypes[record.RouteBlindingEncryptedDataOnionType]

	isNormalHop := !isBlindHop

	// If this is a normal hop, fall back to our usual validation.
	if isNormalHop {
		switch {

		// All hops must include an amount to forward,
		// except those on blinded routes.
		case !hasAmt:
			return ErrInvalidPayload{
				Type:      record.AmtOnionType,
				Violation: OmittedViolation,
				FinalHop:  isFinalHop,
			}

		// All normal hops must include a cltv expiry.
		case !hasLockTime:
			return ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: OmittedViolation,
				FinalHop:  isFinalHop,
			}

		}

	} else {
		// This is a blind hop so we'll apply additional validation
		// as per BOLT-04.
		switch {

		// Intermediate nodes in a blinded route should
		// not contain an amount to forward.
		case !isFinalHop && hasAmt:
			return ErrInvalidPayload{
				Type:      record.AmtOnionType,
				Violation: IncludedViolation,
				FinalHop:  false,
			}

		// Intermediate nodes in a blinded route should
		// not contain an outgoing timelock.
		case !isFinalHop && hasLockTime:
			return ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: IncludedViolation,
				FinalHop:  false,
			}

		}
	}

	// NOTE(11/15/22): The following 3 checks are common for both
	// normal and blind hops.
	switch {
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
