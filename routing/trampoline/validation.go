package trampoline

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// HopRole identifies a trampoline onion hop's position and payment mode.
type HopRole uint8

const (
	// RoleClearIntermediate is a cleartext trampoline hop that routes to
	// another trampoline node.
	RoleClearIntermediate HopRole = iota

	// RoleBolt11Final is the final trampoline hop for a BOLT 11 payment.
	RoleBolt11Final

	// RoleRecipientPaths is the final trampoline hop that was supplied
	// blinded paths for a recipient without trampoline support.
	RoleRecipientPaths

	// RoleBlindedIntroduction is a non-final introduction node in a
	// trampoline-capable blinded path.
	RoleBlindedIntroduction

	// RoleBlindedIntermediate is a non-final, non-introduction node in a
	// trampoline-capable blinded path.
	RoleBlindedIntermediate

	// RoleBlindedFinal is the final, non-introduction node in a
	// trampoline-capable blinded path.
	RoleBlindedFinal

	// RoleBlindedIntroductionFinal is a one-hop blinded path whose
	// introduction node is also the final recipient.
	RoleBlindedIntroductionFinal
)

// String returns a stable name for a trampoline hop role.
func (r HopRole) String() string {
	switch r {
	case RoleClearIntermediate:
		return "clear-intermediate"

	case RoleBolt11Final:
		return "bolt11-final"

	case RoleRecipientPaths:
		return "recipient-paths"

	case RoleBlindedIntroduction:
		return "blinded-introduction"

	case RoleBlindedIntermediate:
		return "blinded-intermediate"

	case RoleBlindedFinal:
		return "blinded-final"

	case RoleBlindedIntroductionFinal:
		return "blinded-introduction-final"

	default:
		return fmt.Sprintf("unknown-role-%d", r)
	}
}

// PayloadViolation identifies how a trampoline payload violated its role.
type PayloadViolation uint8

const (
	// OmittedViolation means a required TLV was absent.
	OmittedViolation PayloadViolation = iota

	// IncludedViolation means a prohibited TLV was present.
	IncludedViolation
)

// String returns a stable name for a payload violation.
func (v PayloadViolation) String() string {
	switch v {
	case OmittedViolation:
		return "omitted"

	case IncludedViolation:
		return "included"

	default:
		return fmt.Sprintf("unknown-violation-%d", v)
	}
}

// ErrInvalidPayload describes a deterministic trampoline payload violation.
type ErrInvalidPayload struct {
	// Role is the role against which the payload was validated.
	Role HopRole

	// Type is the omitted or prohibited TLV type.
	Type tlv.Type

	// Violation says whether Type was omitted or included.
	Violation PayloadViolation
}

// Error returns a human-readable trampoline payload validation error.
func (e ErrInvalidPayload) Error() string {
	return fmt.Sprintf("trampoline payload for %s %s type %d",
		e.Role, e.Violation, e.Type)
}

var (
	// ErrOuterCLTVInsufficient is returned when the outer onion does not
	// provide the expiry promised by the trampoline onion.
	ErrOuterCLTVInsufficient = errors.New(
		"outer cltv below trampoline cltv",
	)

	// ErrOuterAmountInsufficient is returned when a single-part outer
	// onion does not provide the amount promised by the trampoline onion.
	ErrOuterAmountInsufficient = errors.New(
		"outer amount below trampoline amount",
	)

	// ErrOuterTotalInsufficient is returned when a multi-part outer onion's
	// total does not provide the amount promised by the trampoline onion.
	ErrOuterTotalInsufficient = errors.New(
		"outer total below trampoline amount",
	)

	// ErrMissingForwardingInfo is returned when arithmetic is attempted for
	// a payload without its required amount or expiry.
	ErrMissingForwardingInfo = errors.New(
		"trampoline payload missing forwarding information",
	)

	// ErrPathKeyMissing is returned when neither the inner nor outer onion
	// supplies a path key for encrypted recipient data.
	ErrPathKeyMissing = errors.New("trampoline path key missing")

	// ErrPathKeyDuplicated is returned when both the inner and outer onion
	// supply a path key for encrypted recipient data.
	ErrPathKeyDuplicated = errors.New("trampoline path key duplicated")

	// ErrFeeOverflow is returned when trampoline fee arithmetic exceeds
	// the uint64 millisatoshi domain.
	ErrFeeOverflow = errors.New("trampoline fee overflow")

	// ErrCLTVOverflow is returned when a trampoline CLTV calculation
	// exceeds the uint32 block-height domain.
	ErrCLTVOverflow = errors.New("trampoline cltv overflow")
)

type roleRules struct {
	required []tlv.Type
	allowed  map[tlv.Type]struct{}
	strict   bool
}

var knownPayloadTypes = []tlv.Type{
	record.AmtOnionType,
	record.LockTimeOnionType,
	record.MPPOnionType,
	record.EncryptedDataOnionType,
	record.BlindingPointOnionType,
	OutgoingNodeIDType,
	record.MetadataOnionType,
	record.TotalAmtMsatBlindedType,
	OnionPacketType,
	RecipientFeaturesType,
	RecipientBlindedPathsType,
}

// ValidatePayload checks a decoded trampoline payload's field set against its
// exact BOLT 4 role. The parsed map is used so zero-length records remain
// distinguishable from omitted records.
func ValidatePayload(parsed tlv.TypeMap, role HopRole) error {
	rules, err := rulesForRole(role)
	if err != nil {
		return err
	}

	for _, typ := range rules.required {
		if _, ok := parsed[typ]; !ok {
			return ErrInvalidPayload{
				Role:      role,
				Type:      typ,
				Violation: OmittedViolation,
			}
		}
	}

	for _, typ := range knownPayloadTypes {
		if _, ok := parsed[typ]; !ok {
			continue
		}
		if _, ok := rules.allowed[typ]; ok {
			continue
		}

		return ErrInvalidPayload{
			Role:      role,
			Type:      typ,
			Violation: IncludedViolation,
		}
	}

	if !rules.strict {
		return nil
	}

	var (
		foundExtra bool
		minExtra   tlv.Type
	)
	for typ := range parsed {
		if _, ok := rules.allowed[typ]; ok {
			continue
		}
		if !foundExtra || typ < minExtra {
			foundExtra = true
			minExtra = typ
		}
	}
	if foundExtra {
		return ErrInvalidPayload{
			Role:      role,
			Type:      minExtra,
			Violation: IncludedViolation,
		}
	}

	return nil
}

func rulesForRole(role HopRole) (roleRules, error) {
	var (
		required []tlv.Type
		allowed  []tlv.Type
		strict   bool
	)

	switch role {
	case RoleClearIntermediate:
		required = []tlv.Type{
			record.AmtOnionType,
			record.LockTimeOnionType,
			OutgoingNodeIDType,
		}
		allowed = required

	case RoleBolt11Final:
		required = []tlv.Type{
			record.AmtOnionType,
			record.LockTimeOnionType,
			record.MPPOnionType,
		}
		allowed = append([]tlv.Type(nil), required...)
		allowed = append(allowed, record.MetadataOnionType)

	case RoleRecipientPaths:
		required = []tlv.Type{
			record.AmtOnionType,
			record.LockTimeOnionType,
			RecipientBlindedPathsType,
		}
		allowed = append([]tlv.Type(nil), required...)
		allowed = append(allowed, RecipientFeaturesType)

	case RoleBlindedIntroduction:
		required = []tlv.Type{
			record.EncryptedDataOnionType,
			record.BlindingPointOnionType,
		}
		allowed = required
		strict = true

	case RoleBlindedIntermediate:
		required = []tlv.Type{
			record.EncryptedDataOnionType,
		}
		allowed = required
		strict = true

	case RoleBlindedFinal:
		required = []tlv.Type{
			record.AmtOnionType,
			record.LockTimeOnionType,
			record.EncryptedDataOnionType,
			record.TotalAmtMsatBlindedType,
		}
		allowed = required
		strict = true

	case RoleBlindedIntroductionFinal:
		required = []tlv.Type{
			record.AmtOnionType,
			record.LockTimeOnionType,
			record.EncryptedDataOnionType,
			record.BlindingPointOnionType,
			record.TotalAmtMsatBlindedType,
		}
		allowed = required
		strict = true

	default:
		return roleRules{}, fmt.Errorf("unknown trampoline hop role %d",
			role)
	}

	allowedSet := make(map[tlv.Type]struct{}, len(allowed))
	for _, typ := range allowed {
		allowedSet[typ] = struct{}{}
	}

	return roleRules{
		required: required,
		allowed:  allowedSet,
		strict:   strict,
	}, nil
}

// OuterPayment contains the outer onion values checked by a final trampoline
// recipient. TotalAmountMsat is set only for a multi-part payment.
type OuterPayment struct {
	AmountMsat      uint64
	CLTV            uint32
	TotalAmountMsat *uint64
}

// ValidateFinalOuter enforces the final-recipient amount and CLTV constraints
// between the outer and trampoline onions.
func ValidateFinalOuter(outer OuterPayment, inner *Payload) error {
	if inner == nil || inner.AmountToForward == nil ||
		inner.OutgoingCLTV == nil {

		return ErrMissingForwardingInfo
	}

	if outer.CLTV < *inner.OutgoingCLTV {
		return ErrOuterCLTVInsufficient
	}

	if outer.TotalAmountMsat != nil {
		if *outer.TotalAmountMsat < *inner.AmountToForward {
			return ErrOuterTotalInsufficient
		}

		return nil
	}

	if outer.AmountMsat < *inner.AmountToForward {
		return ErrOuterAmountInsufficient
	}

	return nil
}

// SelectPathKey enforces that exactly one of the trampoline or outer onions
// provides the path key used to decrypt encrypted recipient data.
func SelectPathKey(inner, outer *btcec.PublicKey) (*btcec.PublicKey, error) {
	switch {
	case inner == nil && outer == nil:
		return nil, ErrPathKeyMissing

	case inner != nil && outer != nil:
		return nil, ErrPathKeyDuplicated

	case inner != nil:
		return inner, nil

	default:
		return outer, nil
	}
}

// CalculateFee computes base_msat + amount_msat * ppm / 1,000,000 without
// overflowing intermediate arithmetic.
func CalculateFee(amountMsat uint64, baseMsat,
	proportionalMillionths uint32) (uint64, error) {

	ppm := uint64(proportionalMillionths)
	whole := amountMsat / 1_000_000
	remainder := amountMsat % 1_000_000

	if ppm != 0 && whole > math.MaxUint64/ppm {
		return 0, ErrFeeOverflow
	}

	fee := whole*ppm + remainder*ppm/1_000_000
	if fee > math.MaxUint64-uint64(baseMsat) {
		return 0, ErrFeeOverflow
	}

	return fee + uint64(baseMsat), nil
}

// CalculateForwardingAmount reverses a blinded relay fee from the incoming
// amount using the ceiling formula required by BOLT 4.
func CalculateForwardingAmount(incomingMsat uint64, baseMsat,
	proportionalMillionths uint32) (uint64, error) {

	if incomingMsat < uint64(baseMsat) {
		return 0, ErrOuterAmountInsufficient
	}

	denominator := uint64(1_000_000) +
		uint64(proportionalMillionths)
	high, low := bits.Mul64(
		incomingMsat-uint64(baseMsat), 1_000_000,
	)

	var carry uint64
	low, carry = bits.Add64(low, denominator-1, 0)
	high, carry = bits.Add64(high, 0, carry)
	if carry != 0 || high >= denominator {
		return 0, ErrFeeOverflow
	}

	quotient, _ := bits.Div64(high, low, denominator)

	return quotient, nil
}

// RequiredRelayBudget returns the minimum incoming amount and CLTV needed to
// deliver the forwarding values in inner under the supplied relay policy.
func RequiredRelayBudget(inner *Payload, baseMsat,
	proportionalMillionths uint32, cltvDelta uint16) (uint64, uint32,
	error) {

	if inner == nil || inner.AmountToForward == nil ||
		inner.OutgoingCLTV == nil {

		return 0, 0, ErrMissingForwardingInfo
	}

	fee, err := CalculateFee(
		*inner.AmountToForward, baseMsat, proportionalMillionths,
	)
	if err != nil {
		return 0, 0, err
	}
	if *inner.AmountToForward > math.MaxUint64-fee {
		return 0, 0, ErrFeeOverflow
	}
	requiredAmount := *inner.AmountToForward + fee

	if *inner.OutgoingCLTV > math.MaxUint32-uint32(cltvDelta) {
		return 0, 0, ErrCLTVOverflow
	}
	requiredCLTV := *inner.OutgoingCLTV + uint32(cltvDelta)

	return requiredAmount, requiredCLTV, nil
}

// ValidateRelayBudget checks that an incoming outer onion can fund and time
// the downstream route represented by inner and the supplied relay policy.
func ValidateRelayBudget(outer OuterPayment, inner *Payload, baseMsat,
	proportionalMillionths uint32, cltvDelta uint16) error {

	requiredAmount, requiredCLTV, err := RequiredRelayBudget(
		inner, baseMsat, proportionalMillionths, cltvDelta,
	)
	if err != nil {
		return err
	}

	availableAmount := outer.AmountMsat
	if outer.TotalAmountMsat != nil {
		availableAmount = *outer.TotalAmountMsat
	}
	if availableAmount < requiredAmount {
		if outer.TotalAmountMsat != nil {
			return ErrOuterTotalInsufficient
		}

		return ErrOuterAmountInsufficient
	}
	if outer.CLTV < requiredCLTV {
		return ErrOuterCLTVInsufficient
	}

	return nil
}
