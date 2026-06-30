package bolt12

import (
	"errors"
	"fmt"
	"math/bits"
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

	// ErrNilPublicKey is returned when a public-key TLV is present but
	// wraps a nil pointer.
	ErrNilPublicKey = errors.New("public key present but nil")

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

	// ErrMissingAmount is returned when neither offer_amount nor
	// invreq_amount is present.
	ErrMissingAmount = errors.New("missing amount field")

	// ErrAmountBelowExpected is returned when a present invreq_amount is
	// less than the amount expected from offer_amount (times
	// invreq_quantity). The spec states this from both sides: the writer
	// MUST NOT set a lower amount and the reader MUST reject one.
	ErrAmountBelowExpected = errors.New(
		"invreq_amount below offer-expected amount",
	)

	// ErrQuantityZero is returned when invreq_quantity is present but set
	// to 0 while offer_quantity_max is present. The spec distinguishes this
	// from a missing field (see ErrQuantityMissing).
	ErrQuantityZero = errors.New("invreq_quantity is zero")

	// ErrQuantityMissing is returned when offer_quantity_max is present but
	// the invreq omits invreq_quantity entirely. The spec states this as a
	// distinct rejection ("MUST reject ... if there is no invreq_quantity
	// field") from a present-but-zero quantity.
	ErrQuantityMissing = errors.New(
		"invreq_quantity missing but offer_quantity_max present",
	)

	// ErrQuantityExceedsMax is returned when invreq_quantity is greater
	// than offer_quantity_max.
	ErrQuantityExceedsMax = errors.New(
		"invreq_quantity exceeds offer_quantity_max",
	)

	// ErrQuantityWithoutMax is returned when invreq_quantity is set but the
	// mirrored offer_quantity_max is absent. The spec only permits a
	// quantity when the offer advertises a maximum.
	ErrQuantityWithoutMax = errors.New(
		"invreq_quantity set without offer_quantity_max",
	)

	// ErrInvalidBip353Name is returned when invreq_bip_353_name is
	// structurally malformed or contains a non-alphabet byte.
	ErrInvalidBip353Name = errors.New("invalid invreq_bip_353_name")

	// ErrMissingSignature is returned when a wire-form invoice or
	// invoice_request is emitted without a populated signature TLV.
	// Pre-sign Encode (used to compute the Merkle root) is permitted to run
	// without a signature; the bech32 string-codec layer is where the
	// signature becomes mandatory.
	ErrMissingSignature = errors.New("missing signature")

	// ErrOfferFieldsOnSpontaneous is returned when an invoice request
	// is not responding to an offer but includes offer fields (e.g.
	// offer_chains, offer_amount, etc.).
	ErrOfferFieldsOnSpontaneous = errors.New(
		"offer fields present on non-offer response",
	)
)

const (
	// Offer TLV types.
	offerChainsType         tlv.Type = 2
	offerMetadataType       tlv.Type = 4
	offerCurrencyType       tlv.Type = 6
	offerAmountType         tlv.Type = 8
	offerDescriptionType    tlv.Type = 10
	offerFeaturesType       tlv.Type = 12
	offerAbsoluteExpiryType tlv.Type = 14
	offerPathsType          tlv.Type = 16
	offerIssuerType         tlv.Type = 18
	offerQuantityMaxType    tlv.Type = 20
	offerIssuerIDType       tlv.Type = 22

	// InvoiceRequest TLV types.
	invreqMetadataType   tlv.Type = 0
	invreqChainType      tlv.Type = 80
	invreqAmountType     tlv.Type = 82
	invreqFeaturesType   tlv.Type = 84
	invreqQuantityType   tlv.Type = 86
	invreqPayerIDType    tlv.Type = 88
	invreqPayerNoteType  tlv.Type = 89
	invreqPathsType      tlv.Type = 90
	invreqBip353NameType tlv.Type = 91
	signatureTLVType     tlv.Type = 240
)

// isKnownInvreqTLVType determines if a TLV type is defined in the
// invoice_request specification.
func isKnownInvreqTLVType(typ tlv.Type) bool {
	switch typ {
	case invreqMetadataType,
		invreqChainType,
		invreqAmountType,
		invreqFeaturesType,
		invreqQuantityType,
		invreqPayerIDType,
		invreqPayerNoteType,
		invreqPathsType,
		invreqBip353NameType,
		signatureTLVType:

		return true

	default:
		return isKnownOfferTLVType(typ)
	}
}

// ValidateInvoiceRequestWrite ensures an invoice request adheres to the BOLT 12
// writer requirements.
//
// Note: This writer validation assumes that for requests responding to an
// offer, the caller/constructor has already mirrored the offer's fields exactly
// by using the NewInvoiceRequestFromOffer constructor, as an invoice request
// can also be created without an offer.
func ValidateInvoiceRequestWrite(ir *InvoiceRequest) error {
	// A present-but-nil pubkey passes IsSome but would panic the codec on
	// encode, so reject both pubkey fields.
	if err := checkPubKeyNotNil(
		ir.InvreqPayerID, "invreq_payer_id",
	); err != nil {
		return err
	}
	if err := checkPubKeyNotNil(
		ir.OfferIssuerID, "offer_issuer_id",
	); err != nil {
		return err
	}

	// - if it is responding to an offer:
	isResponse := ir.OfferIssuerID.IsSome() || ir.OfferPaths.IsSome()
	//nolint:nestif
	if isResponse {
		// - if offer_chains is set:
		//   - MUST set invreq_chain to one of offer_chains unless that
		//     chain is bitcoin, in which case it SHOULD omit
		//     invreq_chain.
		// - otherwise (no offer_chains):
		//   - if it sets invreq_chain it MUST set it to bitcoin.
		chains := getInvoiceRequestOfferChains(ir)
		chain := getInvreqChain(ir)
		if !slices.Contains(chains, chain) {
			return ErrUnsupportedChain
		}

		// - if offer_amount is not present:
		//   - MUST specify invreq_amount.
		if !ir.OfferAmount.IsSome() && !ir.InvreqAmount.IsSome() {
			return ErrMissingAmount
		}

		// - MUST set signature.sig using the invreq_payer_id.
		// NOT CHECKED HERE: signing happens after this validator runs;
		// the string encoder rejects an unsigned request and the reader
		// verifies signature correctness.

		// - MUST set invreq_payer_id to a transient public key.
		// NOT CHECKED HERE: only presence is checked below; the caller
		// MUST supply a fresh key per request and remember its secret.
		if !ir.InvreqPayerID.IsSome() {
			return ErrMissingPayerID
		}

		// - if offer_quantity_max is present:
		//   - MUST set invreq_quantity to greater than zero.
		//   - if offer_quantity_max is non-zero:
		//     - MUST set invreq_quantity less than or equal to
		//       offer_quantity_max.
		// - otherwise:
		//   - MUST NOT set invreq_quantity
		//
		// Checked before the amount so the bounded quantity feeds the
		// offer_amount*quantity product (reader uses the same order).
		if err := checkInvreqQuantity(ir); err != nil {
			return err
		}

		// - otherwise:
		//   - MAY omit invreq_amount.
		//   - if it sets invreq_amount:
		//     - MUST specify invreq_amount.msat as greater or equal
		//       to amount expected by offer_amount (and, if present,
		//       offer_currency and invreq_quantity).
		if err := checkInvreqAmountMeetsOffer(ir); err != nil {
			return err
		}
	} else {
		// - otherwise (not responding to an offer):

		// - MUST set invreq_payer_id (as it would set offer_issuer_id
		//   for an offer).
		if !ir.InvreqPayerID.IsSome() {
			return ErrMissingPayerID
		}

		// - MUST set invreq_paths as it would set (or not set)
		//   offer_paths for an offer.
		if err := checkBlindedPaths(ir.InvreqPaths); err != nil {
			return err
		}

		// - MUST set offer_description to a complete description of the
		//   purpose of the payment.
		if !ir.OfferDescription.IsSome() {
			return ErrMissingDescription
		}

		// - MUST NOT include signature, offer_metadata, offer_chains,
		//   offer_amount, offer_currency, offer_features,
		//   offer_quantity_max, offer_paths or offer_issuer_id.
		//
		// signature is intentionally omitted from this list: the spec's
		// unsigned offerless variant conflicts with the unconditional
		// reader signature check, and other implementations require a
		// signature on every invoice_request. We always sign, so a
		// present signature here is expected.
		if ir.OfferMetadata.IsSome() || ir.OfferChains.IsSome() ||
			ir.OfferAmount.IsSome() || ir.OfferCurrency.IsSome() ||
			ir.OfferFeatures.IsSome() ||
			ir.OfferQuantityMax.IsSome() ||
			ir.OfferPaths.IsSome() || ir.OfferIssuerID.IsSome() {

			return ErrOfferFieldsOnSpontaneous
		}

		// - if the chain for the invoice is not solely bitcoin:
		//   - MUST specify invreq_chain the offer is valid for.
		// NOT CHECKED HERE: this validator has no chain context. The
		// caller building the request MUST set invreq_chain for
		// non-bitcoin chains; the reader enforces it via activeChain.

		// - MUST NOT set invreq_quantity.
		if err := checkInvreqQuantity(ir); err != nil {
			return err
		}

		// - MUST set invreq_amount.
		if !ir.InvreqAmount.IsSome() {
			return ErrMissingAmount
		}
	}

	// - MUST NOT set any non-signature TLV fields outside the inclusive
	//   ranges: 0 to 159 and 1000000000 to 2999999999
	//
	// The signature range (240-1000) is excluded: it carries signature TLV
	// elements, which by design sit outside the message ranges.
	for _, t := range sortedTypes(ir.decodedTLVs) {
		if bolt12InUnsignedRange(t) {
			continue
		}
		if !invreqAllowedRange(t) {
			return fmt.Errorf("%w: type %d",
				ErrOutOfRangeType, t)
		}
	}

	// - MUST set invreq_metadata to an unpredictable series of bytes.
	// NOT CHECKED HERE: only presence is verified; unpredictability is the
	// caller's responsibility.
	if !ir.InvreqMetadata.IsSome() {
		return ErrMissingMetadata
	}

	// - if it sets invreq_amount: MUST set msat in multiples of the minimum
	//   payable unit.
	// NOT CHECKED HERE: trivially satisfied for bitcoin (msat); a caller
	// on a chain with a coarser unit MUST enforce it.

	// - if it supports bolt12 invoice request features:
	//   - MUST set invreq_features.features to the bitmap of features.
	// We only reject unknown even bits here; advertising a feature is the
	// caller's decision. Since the writer lacks a catalogue in scope, we
	// pass nil for the catalogue, treating all even feature bits as
	// unknown.
	if err := checkFeatures(ir.InvreqFeatures, nil); err != nil {
		return err
	}

	// check UTF-8 constraints and BIP 353
	err := checkUTF8(ir.InvreqPayerNote, "invreq_payer_note")
	if err != nil {
		return err
	}

	// - if it received the offer using BIP 353 resolution:
	//   - MUST include invreq_bip_353_name with name/domain from the HRN.
	// NOT CHECKED HERE: whether resolution was used is caller context; we
	// only validate the field's alphabet/layout when present.
	if err := checkBip353Name(ir.InvreqBip353Name); err != nil {
		return err
	}

	return nil
}

// invreqAllowedRange determines if the TLV type falls within the allowed
// ranges for invoice request messages.
func invreqAllowedRange(typ tlv.Type) bool {
	return typ <= 159 ||
		(typ >= 1000000000 && typ <= 2999999999)
}

// checkInvreqAmountMeetsOffer enforces that a present invreq_amount is at least
// offer_amount times invreq_quantity. Shared by the writer and reader, which
// state the same rule from each side.
//
// It covers only the native case (offer_currency absent, amounts in msat). When
// offer_currency is present the expected amount needs a live exchange-rate
// conversion the codec cannot do, so the caller MUST compare; it returns nil.
func checkInvreqAmountMeetsOffer(ir *InvoiceRequest) error {
	if !ir.OfferAmount.IsSome() || !ir.InvreqAmount.IsSome() {
		return nil
	}

	// NOT CHECKED HERE: the offer_currency (non-bitcoin) case. Caller MUST
	// convert offer_amount to the invreq_chain currency and compare.
	if ir.OfferCurrency.IsSome() {
		return nil
	}

	var offerAmt uint64
	ir.OfferAmount.WhenSome(func(r tlv.RecordT[tlv.TlvType8, TUint64]) {
		offerAmt = uint64(r.Val)
	})

	var qty uint64 = 1
	ir.InvreqQuantity.WhenSome(func(r tlv.RecordT[tlv.TlvType86, TUint64]) {
		qty = uint64(r.Val)
	})

	var invreqAmt uint64
	ir.InvreqAmount.WhenSome(func(r tlv.RecordT[tlv.TlvType82, TUint64]) {
		invreqAmt = uint64(r.Val)
	})

	// Guard against overflows.
	hi, expectedAmt := bits.Mul64(offerAmt, qty)
	if hi != 0 {
		return fmt.Errorf("%w: offer_amount %d * quantity %d "+
			"overflows uint64", ErrAmountBelowExpected,
			offerAmt, qty)
	}
	if invreqAmt < expectedAmt {
		return fmt.Errorf("%w: invreq_amount %d below expected %d",
			ErrAmountBelowExpected, invreqAmt, expectedAmt)
	}

	return nil
}

// getInvreqChain returns the chain genesis hash an invoice request
// targets, defaulting to Bitcoin mainnet when invreq_chain is absent
// per the BOLT 12 reader rule.
func getInvreqChain(ir *InvoiceRequest) [32]byte {
	chain := bitcoinMainnetGenesisHash
	ir.InvreqChain.WhenSome(
		func(r tlv.RecordT[tlv.TlvType80, [32]byte]) {
			chain = r.Val
		},
	)

	return chain
}

// ValidateInvoiceRequestRead validates an invoice request against the BOLT 12
// reader requirements. It performs generic, stateless structural checks only.
// Stateful or contextual checks (offer matching, path verification, unit-price
// calculations) must be handled externally by the caller.
//
// Signature verification is NOT performed yet: the reader MUST also reject a
// request whose Schnorr signature does not verify against invreq_payer_id, but
// that check is deferred until the merkle/signing primitives land with the
// Invoice message (see the TODO at the end of this function). Until then, a
// caller wiring this into a handler MUST verify the signature itself.
func ValidateInvoiceRequestRead(ir *InvoiceRequest,
	activeChain [32]byte,
	knownFeatures map[lnwire.FeatureBit]string) error {

	// A present-but-nil pubkey passes IsSome but would panic the codec on
	// encode, so reject both pubkey fields.
	if err := checkPubKeyNotNil(
		ir.InvreqPayerID, "invreq_payer_id",
	); err != nil {
		return err
	}
	if err := checkPubKeyNotNil(
		ir.OfferIssuerID, "offer_issuer_id",
	); err != nil {
		return err
	}

	// - MUST reject the invoice request if invreq_payer_id or
	// invreq_metadata are not present.
	if !ir.InvreqPayerID.IsSome() {
		return ErrMissingPayerID
	}
	if !ir.InvreqMetadata.IsSome() {
		return ErrMissingMetadata
	}

	// - MUST reject the invoice request if any non-signature TLV fields are
	//   outside the inclusive ranges: 0 to 159 and 1000000000 to 2999999999
	//
	// The signature range (240-1000) is excluded from the out-of-range
	// check: it holds one or more signature TLV elements, so an unknown odd
	// type there is a future optional signature we ignore ("it's ok to be
	// odd") rather than reject. Unknown even types remain must-understand
	// and are rejected everywhere, including inside the signature range.
	for _, t := range sortedTypes(ir.decodedTLVs) {
		if !bolt12InUnsignedRange(t) && !invreqAllowedRange(t) {
			return fmt.Errorf("%w: type %d",
				ErrOutOfRangeType, t)
		}
		if !isKnownInvreqTLVType(t) && t%2 == 0 {
			return fmt.Errorf("%w: type %d",
				ErrUnknownEvenType, t)
		}
	}

	// - if invreq_features contains unknown *even* bits that are non-zero:
	//   - MUST reject the invoice request.
	if err := checkFeatures(ir.InvreqFeatures, knownFeatures); err != nil {
		return err
	}

	// - if num_hops is 0 in any blinded_path in invreq_paths:
	//   - MUST reject the invoice request.
	if err := checkBlindedPaths(ir.InvreqPaths); err != nil {
		return err
	}

	// - if offer_issuer_id or offer_paths are present (response to an
	// offer):
	isResponse := ir.OfferIssuerID.IsSome() || ir.OfferPaths.IsSome()
	if isResponse {
		// NOT CHECKED HERE (need the offer store / arrival path /
		// reply-path state, so the caller MUST do these):
		//   - MUST reject if the offer fields do not exactly match a
		//     valid, unexpired offer.
		//   - if offer_paths is present: MUST ignore the request unless
		//     it arrived via one of those paths; otherwise MUST ignore
		//     any request that arrived via a blinded path.
		//   - if invreq_metadata equals a previous request: MAY reply
		//     with the previous invoice; otherwise MUST NOT.
		//   - SHOULD send the invoice via the onionmsg_tlv reply_path.

		// - if offer_quantity_max is present:
		//   - MUST reject the invoice request if there is no
		//     invreq_quantity field.
		// - if offer_quantity_max is non-zero:
		//   - MUST reject the invoice request if invreq_quantity is
		//     zero, OR greater than offer_quantity_max.
		// - otherwise (no offer_quantity_max):
		//   - MUST reject the invoice request if there is an
		//     invreq_quantity field.
		if err := checkInvreqQuantity(ir); err != nil {
			return err
		}

		// - if offer_amount is present: if invreq_amount is present,
		//   MUST reject when it is below the expected amount. The
		//   helper covers the native case; the currency-conversion
		//   case is deferred to the caller (see the helper doc).
		if err := checkInvreqAmountMeetsOffer(ir); err != nil {
			return err
		}

		if !ir.OfferAmount.IsSome() {
			// - otherwise (no offer_amount):
			//   - MUST reject the invoice request if it does not
			//     contain invreq_amount.
			if !ir.InvreqAmount.IsSome() {
				return ErrMissingAmount
			}
		}
	} else {
		// - otherwise (no offer_issuer_id or offer_paths, not a
		//   response to our offer):

		// - MUST reject the invoice request if any of the following
		//   are present: offer_chains, offer_features or
		//   offer_quantity_max.
		if ir.OfferChains.IsSome() || ir.OfferFeatures.IsSome() ||
			ir.OfferQuantityMax.IsSome() {

			return ErrOfferFieldsOnSpontaneous
		}

		// - MUST reject the invoice request if there is an
		//   invreq_quantity field.
		if err := checkInvreqQuantity(ir); err != nil {
			return err
		}

		// - MUST reject the invoice request if invreq_amount is not
		//   present.
		if !ir.InvreqAmount.IsSome() {
			return ErrMissingAmount
		}

		// NOT CHECKED HERE (caller's responsibility if it replies):
		//   - MAY use offer_amount / offer_currency for informational
		//     display to the user.
		//   - if it sends an invoice in response: MUST use invreq_paths
		//     if present, otherwise MUST use invreq_payer_id as
		//     the node id to send to.
	}

	// - if invreq_chain is not present:
	//   - MUST reject the invoice request if bitcoin is not a supported
	//     chain.
	// - otherwise:
	//   - MUST reject the invoice request if invreq_chain.chain is not a
	//     supported chain.
	if getInvreqChain(ir) != activeChain {
		return ErrUnsupportedChain
	}

	// - if invreq_bip_353_name is present:
	//   - MUST reject the invoice request if name or domain contain any
	//     bytes which are not 0-9, a-z, A-Z, -, _ or .
	if err := checkBip353Name(ir.InvreqBip353Name); err != nil {
		return err
	}

	// - MUST reject the invoice request if signature is not correct as
	//   detailed in Signature Calculation using the invreq_payer_id.
	// TODO(bolt12): implement signature verification.
	if !ir.Signature.IsSome() {
		return ErrMissingSignature
	}

	return nil
}

// getInvoiceRequestOfferChains returns the chains an invoice request's mirrored
// offer is valid for. If offer_chains is absent, the spec defaults to Bitcoin
// mainnet.
func getInvoiceRequestOfferChains(ir *InvoiceRequest) [][32]byte {
	chains := fn.MapOptionZ(
		ir.OfferChains.ValOpt(),
		func(r ChainsRecord) [][32]byte { return r.Chains },
	)

	if len(chains) == 0 {
		chains = [][32]byte{bitcoinMainnetGenesisHash}
	}

	return chains
}

// checkInvreqQuantity validates the spec coupling between offer_quantity_max
// and invreq_quantity.
func checkInvreqQuantity(ir *InvoiceRequest) error {
	// Without offer_quantity_max the spec forbids invreq_quantity: the
	// writer MUST NOT set it and the reader MUST reject a request that
	// carries it.
	if !ir.OfferQuantityMax.IsSome() {
		if ir.InvreqQuantity.IsSome() {
			return ErrQuantityWithoutMax
		}

		return nil
	}

	// offer_quantity_max is present, so invreq_quantity is mandatory. The
	// spec separates "no invreq_quantity field" from "invreq_quantity is
	// zero", so report them with distinct sentinels even though both
	// reject.
	if !ir.InvreqQuantity.IsSome() {
		return ErrQuantityMissing
	}

	var qty uint64
	ir.InvreqQuantity.WhenSome(
		func(r tlv.RecordT[tlv.TlvType86, TUint64]) {
			qty = uint64(r.Val)
		},
	)
	if qty == 0 {
		return ErrQuantityZero
	}

	var maxQty uint64
	ir.OfferQuantityMax.WhenSome(
		func(r tlv.RecordT[tlv.TlvType20, TUint64]) {
			maxQty = uint64(r.Val)
		},
	)

	// If maxQty is 0 (unlimited/unknown), we only enforce that the
	// requested qty is greater than zero, bypassing the upper bound check.
	if maxQty > 0 && qty > maxQty {
		return ErrQuantityExceedsMax
	}

	return nil
}

// checkBip353Name validates the wire layout and alphabet of
// invreq_bip_353_name. Both name and domain MUST contain only DNS-safe
// characters per the BOLT 12 reader and writer requirements.
func checkBip353Name(opt tlv.OptionalRecordT[tlv.TlvType91, tlv.Blob]) error {
	var (
		data    []byte
		present bool
	)
	opt.WhenSome(func(r tlv.RecordT[tlv.TlvType91, tlv.Blob]) {
		data = r.Val
		present = true
	})

	// An absent field is a no-op. A present-but-empty field is malformed
	// (it cannot carry name_len) and falls through to the length check
	// below rather than being mistaken for absent.
	if !present {
		return nil
	}

	if len(data) < 1 {
		return fmt.Errorf("%w: missing name_len", ErrInvalidBip353Name)
	}
	nameLen := int(data[0])
	if nameLen == 0 {
		return fmt.Errorf("%w: empty name", ErrInvalidBip353Name)
	}

	domainLenIdx := 1 + nameLen
	if domainLenIdx >= len(data) {
		return fmt.Errorf("%w: truncated before domain_len",
			ErrInvalidBip353Name)
	}

	name := data[1:domainLenIdx]

	domainStart := domainLenIdx + 1
	domainLen := int(data[domainLenIdx])
	if domainLen == 0 {
		return fmt.Errorf("%w: empty domain", ErrInvalidBip353Name)
	}
	if domainStart+domainLen != len(data) {
		return fmt.Errorf("%w: domain length mismatch",
			ErrInvalidBip353Name)
	}
	domain := data[domainStart:]

	if err := checkBip353Alphabet(name); err != nil {
		return fmt.Errorf("%w: name: %w",
			ErrInvalidBip353Name, err)
	}

	if err := checkBip353Alphabet(domain); err != nil {
		return fmt.Errorf("%w: domain: %w",
			ErrInvalidBip353Name, err)
	}

	return nil
}

// checkBip353Alphabet returns an error when any byte falls outside the BIP 353
// alphabet.
func checkBip353Alphabet(b []byte) error {
	for i, c := range b {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_' || c == '.':
		default:
			return fmt.Errorf("byte %d (0x%02x) outside "+
				"alphabet", i, c)
		}
	}

	return nil
}

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
	case offerChainsType,
		offerMetadataType,
		offerCurrencyType,
		offerAmountType,
		offerDescriptionType,
		offerFeaturesType,
		offerAbsoluteExpiryType,
		offerPathsType,
		offerIssuerType,
		offerQuantityMaxType,
		offerIssuerIDType:

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
func ValidateOfferRead(o *Offer, now time.Time, activeChain [32]byte,
	knownFeatures map[lnwire.FeatureBit]string) error {

	// A present-but-nil offer_issuer_id passes IsSome but would panic the
	// codec on encode, so reject it here.
	if err := checkPubKeyNotNil(
		o.OfferIssuerID, "offer_issuer_id",
	); err != nil {
		return err
	}
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
	if err := checkFeatures(o.OfferFeatures, knownFeatures); err != nil {
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
	// A present-but-nil offer_issuer_id passes IsSome but would panic the
	// codec on encode, so reject it here.
	if err := checkPubKeyNotNil(
		o.OfferIssuerID, "offer_issuer_id",
	); err != nil {
		return err
	}

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
	opt tlv.OptionalRecordT[T, lnwire.RawFeatureVector],
	known map[lnwire.FeatureBit]string) error {

	return fn.MapOptionZ(
		opt.ValOpt(),
		func(fv lnwire.RawFeatureVector) error {
			wrapped := lnwire.NewFeatureVector(&fv, known)
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

// checkPubKeyNotNil returns an error if a public key TLV is present but nil.
func checkPubKeyNotNil[T tlv.TlvType](
	opt tlv.OptionalRecordT[T, *btcec.PublicKey], name string) error {

	return fn.MapOptionZ(opt.ValOpt(), func(pk *btcec.PublicKey) error {
		if pk == nil {
			return fmt.Errorf("%w: %s", ErrNilPublicKey, name)
		}

		return nil
	})
}
