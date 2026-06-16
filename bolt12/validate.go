package bolt12

import (
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"golang.org/x/text/currency"
)

var (
	// ErrOutOfRangeType is returned when a TLV type falls outside the
	// allowed offer ranges (1-79 and 1000000000-1999999999).
	ErrOutOfRangeType = errors.New("TLV type outside allowed range")

	// ErrUnknownEvenType is returned when an unknown even TLV type is
	// present in an allowed range. Per BOLT 1, even types are
	// must-understand: if the reader does not recognise the type, it MUST
	// reject the message rather than silently ignoring the field.
	ErrUnknownEvenType = errors.New("unknown even TLV type")

	// ErrUnknownEvenFeature is returned when an unknown even feature
	// bit is set.
	ErrUnknownEvenFeature = errors.New("unknown even feature bit set")

	// ErrMissingDescription is returned when offer_amount is set but
	// offer_description is absent.
	ErrMissingDescription = errors.New(
		"offer_amount set without offer_description",
	)

	// ErrCurrencyWithoutAmount is returned when offer_currency is set
	// but offer_amount is absent.
	ErrCurrencyWithoutAmount = errors.New(
		"offer_currency set without offer_amount",
	)

	// ErrZeroAmount is returned when offer_amount is set to zero. The spec
	// requires a present offer_amount to be strictly greater than zero so
	// that a zero-value cannot masquerade as "no minimum required".
	ErrZeroAmount = errors.New("offer_amount must be greater than zero")

	// ErrEmptyBlindedPaths is returned when a blinded paths field is
	// present on a BOLT 12 message but its list of paths is empty. The
	// spec writer requirements treat "present" as implying at least one
	// usable path.
	ErrEmptyBlindedPaths = errors.New("blinded paths field present but " +
		"empty")

	// ErrNoIssuerIdentity is returned when neither offer_issuer_id
	// nor offer_paths is set.
	ErrNoIssuerIdentity = errors.New(
		"neither offer_issuer_id nor offer_paths set",
	)

	// ErrOfferExpired is returned when the current time is after
	// offer_absolute_expiry.
	ErrOfferExpired = errors.New("offer has expired")

	// ErrEmptyChains is returned when offer_chains is present but
	// contains no entries.
	ErrEmptyChains = errors.New(
		"offer_chains present but empty",
	)

	// ErrUnsupportedChain is returned when offer_chains does not
	// contain our active chain.
	ErrUnsupportedChain = errors.New(
		"offer does not support our chain",
	)

	// ErrInvalidUTF8 is returned when a UTF-8 field contains invalid
	// sequences.
	ErrInvalidUTF8 = errors.New("invalid UTF-8")

	// ErrInvalidCurrency is returned when offer_currency is not a valid ISO
	// 4217 code.
	ErrInvalidCurrency = errors.New("invalid offer_currency")
)

// offerAllowedRange returns true if the TLV type falls within the allowed
// ranges for offer messages: 1-79 and 1000000000-1999999999.
func offerAllowedRange(typ tlv.Type) bool {
	return (typ >= 1 && typ <= 79) ||
		(typ >= 1000000000 && typ <= 1999999999)
}

// isKnownOfferTLVType returns true for TLV types that are defined in the offer
// spec (even types 2-22).
func isKnownOfferTLVType(typ tlv.Type) bool {
	switch typ {
	case 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22:
		return true
	default:
		return false
	}
}

// ValidateOfferRead validates an offer per the BOLT 12 offer reader
// requirements. The now parameter is used for expiry checks and can be
// overridden in tests. activeChain is required: per spec, absent offer_chains
// defaults to Bitcoin mainnet, and the reader must reject offers that do not
// list a chain it operates on. Pass the genesis hash of the chain the receiver
// is willing to settle on.
func ValidateOfferRead(o *Offer, now time.Time, activeChain [32]byte) error {
	// Check TLV types are in allowed range and that unknown even types are
	// rejected (even = must-understand).
	for _, t := range sortedTypes(o.decodedTLVs) {
		if !offerAllowedRange(t) {
			return fmt.Errorf("%w: type %d", ErrOutOfRangeType, t)
		}

		if !isKnownOfferTLVType(t) && t%2 == 0 {
			return fmt.Errorf("%w: type %d", ErrUnknownEvenType, t)
		}
	}

	// Check for unknown even feature bits.
	if err := checkFeatures(o.OfferFeatures); err != nil {
		return err
	}

	// offer_chains present but empty.
	var chainsEmpty bool
	o.OfferChains.WhenSome(
		func(r tlv.RecordT[tlv.TlvType2, ChainsRecord]) {
			if len(r.Val.Chains) == 0 {
				chainsEmpty = true
			}
		},
	)
	if chainsEmpty {
		return ErrEmptyChains
	}

	// Validate the offer's chain against the active chain. An absent
	// offer_chains TLV means "Bitcoin mainnet" per spec, normalised by
	// getOfferChains.
	offerChains := getOfferChains(o)
	found := slices.Contains(offerChains, activeChain)
	if !found {
		return ErrUnsupportedChain
	}

	// offer_amount set requires offer_description.
	hasAmount := o.OfferAmount.IsSome()
	if hasAmount && !o.OfferDescription.IsSome() {
		return ErrMissingDescription
	}

	// offer_amount, if set, must be strictly greater than zero.
	if err := checkAmountPositive(o.OfferAmount); err != nil {
		return err
	}

	// offer_currency requires offer_amount.
	if o.OfferCurrency.IsSome() && !hasAmount {
		return ErrCurrencyWithoutAmount
	}

	// Must have either offer_issuer_id or offer_paths.
	if !o.OfferIssuerID.IsSome() && !o.OfferPaths.IsSome() {
		return ErrNoIssuerIdentity
	}

	// Check blinded paths have at least one hop.
	if err := checkBlindedPaths(o.OfferPaths); err != nil {
		return err
	}

	// Expiry check. A present-but-zero offer_absolute_expiry is as a valid
	// timestamp in the past, it doesn't have the special meaning of "no
	// expiry".
	var (
		expiry    uint64
		hasExpiry bool
	)
	o.OfferAbsoluteExpiry.WhenSome(
		func(r tlv.RecordT[tlv.TlvType14, TUint64]) {
			expiry = uint64(r.Val)
			hasExpiry = true
		},
	)
	if hasExpiry && uint64(now.Unix()) > expiry {
		return ErrOfferExpired
	}

	// Validate UTF-8 fields.
	if err := checkUTF8(o.OfferCurrency, "offer_currency"); err != nil {
		return err
	}

	if err := checkUTF8(
		o.OfferDescription, "offer_description",
	); err != nil {
		return err
	}

	if err := checkUTF8(o.OfferIssuer, "offer_issuer"); err != nil {
		return err
	}

	if err := checkISO4217(o.OfferCurrency); err != nil {
		return err
	}

	return nil
}

// bitcoinMainnetGenesisHash is the genesis hash for Bitcoin mainnet, used as
// the default when offer_chains is absent per the spec.
var bitcoinMainnetGenesisHash = [32]byte(*chaincfg.MainNetParams.GenesisHash)

// getOfferChains returns the chains an offer is valid for. If offer_chains is
// absent, the spec defaults to Bitcoin mainnet.
func getOfferChains(o *Offer) [][32]byte {
	chains := fn.MapOptionZ(
		o.OfferChains.ValOpt(),
		func(r ChainsRecord) [][32]byte { return r.Chains },
	)

	if len(chains) == 0 {
		chains = [][32]byte{bitcoinMainnetGenesisHash}
	}

	return chains
}

// ValidateOfferWrite validates an offer per the BOLT 12 offer writer
// requirements.
func ValidateOfferWrite(o *Offer) error {
	// Writer MUST NOT set TLV fields outside allowed ranges. This check
	// catches a decoded-then-mutated offer: a freshly-built struct has no
	// decodedTLVs (Decode is the only writer of that field). The typed
	// field set already excludes out-of-range types by construction, so a
	// freshly-built offer cannot violate the range rule in the first place.
	for _, t := range sortedTypes(o.decodedTLVs) {
		if !offerAllowedRange(t) {
			return fmt.Errorf("%w: type %d",
				ErrOutOfRangeType, t)
		}
	}

	// offer_amount requires offer_description.
	if o.OfferAmount.IsSome() && !o.OfferDescription.IsSome() {
		return ErrMissingDescription
	}

	// offer_amount, if set, must be strictly greater than zero.
	if err := checkAmountPositive(o.OfferAmount); err != nil {
		return err
	}

	// offer_currency requires offer_amount.
	if o.OfferCurrency.IsSome() && !o.OfferAmount.IsSome() {
		return ErrCurrencyWithoutAmount
	}

	// Without offer_paths, MUST set offer_issuer_id.
	if !o.OfferPaths.IsSome() && !o.OfferIssuerID.IsSome() {
		return ErrNoIssuerIdentity
	}

	// A present-but-nil offer_issuer_id would panic in SerializeCompressed
	// at encode time, so reject it here.
	if err := fn.MapOptionZ(
		o.OfferIssuerID.ValOpt(),
		func(pk *btcec.PublicKey) error {
			if pk == nil {
				return fmt.Errorf("nil issuer public key")
			}

			return nil
		},
	); err != nil {
		return err
	}

	// Defense in depth: writer-side mirrors of reader rejections for
	// present-but-empty offer_chains and offer_paths.
	var chainsEmpty bool
	o.OfferChains.WhenSome(
		func(r tlv.RecordT[tlv.TlvType2, ChainsRecord]) {
			if len(r.Val.Chains) == 0 {
				chainsEmpty = true
			}
		},
	)
	if chainsEmpty {
		return ErrEmptyChains
	}

	if err := checkBlindedPaths(o.OfferPaths); err != nil {
		return err
	}

	// Defense in depth: writer-side mirrors of the reader UTF-8 checks
	// for offer_currency, offer_description, and offer_issuer.
	if err := checkUTF8(o.OfferCurrency, "offer_currency"); err != nil {
		return err
	}

	if err := checkUTF8(
		o.OfferDescription, "offer_description",
	); err != nil {
		return err
	}

	if err := checkUTF8(o.OfferIssuer, "offer_issuer"); err != nil {
		return err
	}

	if err := checkISO4217(o.OfferCurrency); err != nil {
		return err
	}

	return nil
}

// checkISO4217 verifies that offer_currency, if set, parses as an ISO 4217
// code. The upstream parser is case-insensitive and rejects both malformed and
// unrecognised codes.
func checkISO4217[T tlv.TlvType](opt tlv.OptionalRecordT[T, tlv.Blob]) error {
	return fn.MapOptionZ(opt.ValOpt(), func(data tlv.Blob) error {
		if _, err := currency.ParseISO(string(data)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidCurrency, err)
		}

		return nil
	})
}

// checkFeatures rejects any unknown even (must-understand) feature bit.
func checkFeatures[T tlv.TlvType](
	opt tlv.OptionalRecordT[T, lnwire.RawFeatureVector]) error {

	return fn.MapOptionZ(
		opt.ValOpt(),
		func(fv lnwire.RawFeatureVector) error {
			// nil catalogue: BOLT 12 defines no feature bits yet,
			// so every set even bit is "unknown". Swap in a
			// Bolt12Features map once the spec assigns bits.
			wrapped := lnwire.NewFeatureVector(&fv, nil)
			unknown := wrapped.UnknownRequiredFeatures()
			if len(unknown) == 0 {
				return nil
			}

			// Sort for deterministic errors.
			slices.Sort(unknown)

			return fmt.Errorf("%w: bit %d",
				ErrUnknownEvenFeature, unknown[0])
		},
	)
}

// checkBlindedPaths walks each path in a blinded paths field and rejects empty
// Paths slices and paths with zero hops.
func checkBlindedPaths[T tlv.TlvType](
	opt tlv.OptionalRecordT[T, lnwire.BlindedPaths]) error {

	return fn.MapOptionZ(
		opt.ValOpt(),
		func(paths lnwire.BlindedPaths) error {
			if len(paths.Paths) == 0 {
				return ErrEmptyBlindedPaths
			}

			for i, p := range paths.Paths {
				if len(p.Hops) == 0 {
					return fmt.Errorf("%w: path %d",
						lnwire.ErrEmptyBlindedPath, i)
				}
			}

			return nil
		},
	)
}

// checkAmountPositive rejects an offer_amount that is present but zero.
func checkAmountPositive[T tlv.TlvType](
	opt tlv.OptionalRecordT[T, TUint64]) error {

	return fn.MapOptionZ(opt.ValOpt(), func(v TUint64) error {
		if v == 0 {
			return ErrZeroAmount
		}

		return nil
	})
}

// checkUTF8 validates that a blob field contains valid UTF-8.
func checkUTF8[T tlv.TlvType](opt tlv.OptionalRecordT[T, tlv.Blob],
	name string) error {

	return fn.MapOptionZ(opt.ValOpt(), func(data tlv.Blob) error {
		if !utf8.Valid(data) {
			return fmt.Errorf("%w: %s", ErrInvalidUTF8, name)
		}

		return nil
	})
}
