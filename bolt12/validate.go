package bolt12

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
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

	// ErrMissingCreatedAt is returned when invoice_created_at is absent.
	ErrMissingCreatedAt = errors.New("missing invoice_created_at")

	// ErrMissingPaymentHash is returned when invoice_payment_hash is
	// absent.
	ErrMissingPaymentHash = errors.New("missing invoice_payment_hash")

	// ErrMissingNodeID is returned when invoice_node_id is absent.
	ErrMissingNodeID = errors.New("missing invoice_node_id")

	// ErrMissingBlindedPay is returned when invoice_blindedpay is absent.
	ErrMissingBlindedPay = errors.New("missing invoice_blindedpay")

	// ErrBlindedPayMismatch is returned when invoice_blindedpay does not
	// correspond 1:1 with invoice_paths.
	ErrBlindedPayMismatch = errors.New(
		"invoice_blindedpay count does not match invoice_paths",
	)

	// ErrMissingPaths is returned when invoice_paths is absent.
	ErrMissingPaths = errors.New("missing invoice_paths")

	// ErrNoUsablePaths is returned by ValidateInvoiceRead when every
	// blinded path in invoice_paths carries unknown required features in
	// payinfo.
	ErrNoUsablePaths = errors.New(
		"no blinded paths with known required features",
	)

	// ErrInvoiceExpired is returned by ValidateInvoiceExpiry when the
	// caller's clock is past invoice_created_at + invoice_relative_expiry
	// (default 7200 seconds when relative expiry is absent).
	ErrInvoiceExpired = errors.New("invoice has expired")

	// ErrInvoiceMismatch is returned when an invoice field does not match
	// the invoice request.
	ErrInvoiceMismatch = errors.New(
		"invoice field mismatch with request",
	)

	// ErrInvoiceNodeIDMismatch is returned when offer_issuer_id is present
	// but invoice_node_id does not equal it. The spec requires the invoice
	// to be signed by the offer's issuer in this case.
	ErrInvoiceNodeIDMismatch = errors.New(
		"invoice_node_id does not match offer_issuer_id",
	)

	// ErrZeroInvoiceAmount is returned when invoice_amount is present but
	// set to zero. The spec permits a zero "minimum amount", but a
	// zero-amount HTLC cannot settle past the channel-layer dust limit, so
	// the codec rejects it with a typed sentinel a spec-strict caller can
	// distinguish from a missing-field violation.
	ErrZeroInvoiceAmount = errors.New("invoice_amount must be greater " +
		"than zero")

	// ErrMissingError is returned when an invoice_error omits the error
	// field.
	ErrMissingError = errors.New("invoice_error missing error field")

	// ErrEmptyError is returned when an invoice_error carries a zero-length
	// error field.
	ErrEmptyError = errors.New(
		"invoice_error error field is empty",
	)

	// ErrSuggestedWithoutField is returned when suggested_value is set
	// without erroneous_field.
	ErrSuggestedWithoutField = errors.New(
		"suggested_value set without erroneous_field",
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

	// Invoice TLV types.
	invoicePathsType          tlv.Type = 160
	invoiceBlindedPayType     tlv.Type = 162
	invoiceCreatedAtType      tlv.Type = 164
	invoiceRelativeExpiryType tlv.Type = 166
	invoicePaymentHashType    tlv.Type = 168
	invoiceAmountType         tlv.Type = 170
	invoiceFallbacksType      tlv.Type = 172
	invoiceFeaturesType       tlv.Type = 174
	invoiceNodeIDType         tlv.Type = 176

	// InvoiceError TLV types.
	invoiceErrorErroneousFieldType tlv.Type = 1
	invoiceErrorSuggestedValueType tlv.Type = 3
	invoiceErrorErrorType          tlv.Type = 5
)

// ValidateInvoiceErrorWrite validates an invoice_error per the BOLT 12 writer
// requirements. The checks follow the spec's writer section in order. The
// caller must check that the suggested value, if present, contains a valid
// type.
func ValidateInvoiceErrorWrite(ie *InvoiceError) error {
	// - MUST set error to an explanatory string.
	if !ie.Error.IsSome() {
		return ErrMissingError
	}

	// The spec's "explanatory string" is a type-5 UTF-8 message, so an
	// empty or non-UTF-8 blob carries no explanation and fails the
	// requirement even though IsSome passes. checkUTF8 mirrors the
	// treatment every other BOLT 12 UTF-8 field receives.
	if len(ie.Error.ValOpt().UnwrapOr(nil)) == 0 {
		return ErrEmptyError
	}
	if err := checkUTF8(ie.Error, "error"); err != nil {
		return err
	}

	// - MAY set erroneous_field to a specific field number in the invoice
	//   or invoice_request which had a problem.
	// No presence check: erroneous_field is optional for the writer.
	hasErrField := ie.ErroneousField.IsSome()

	// - if it sets erroneous_field:
	//   - MAY set suggested_value.
	// - otherwise:
	//   - MUST NOT set suggested_value.
	if ie.SuggestedValue.IsSome() && !hasErrField {
		return ErrSuggestedWithoutField
	}

	//   - if it sets suggested_value:
	//     - MUST set suggested_value to a valid field for that
	//       tlv_fieldnum.
	// NOT CHECKED HERE: verifying the replacement is a valid encoding for
	// the erroneous field needs the schema of the rejected invoice or
	// invoice_request, which is caller context this validator does not
	// have.

	return nil
}

// isKnownInvoiceErrorTLVType reports whether typ is a defined invoice_error
// TLV type (1, 3, 5).
func isKnownInvoiceErrorTLVType(typ tlv.Type) bool {
	switch typ {
	case invoiceErrorErroneousFieldType,
		invoiceErrorSuggestedValueType,
		invoiceErrorErrorType:

		return true
	default:
		return false
	}
}

// ValidateInvoiceErrorRead applies the BOLT 1 must-understand rule to a decoded
// invoice_error: reject unknown even TLV types, tolerate unknown odd ones. It
// is the only read-side check, since BOLT 12 leaves the semantic reader
// requirements undefined (FIXME).
func ValidateInvoiceErrorRead(ie *InvoiceError) error {
	for _, t := range sortedTypes(ie.decodedTLVs) {
		if !isKnownInvoiceErrorTLVType(t) && t%2 == 0 {
			return fmt.Errorf("%w: type %d", ErrUnknownEvenType, t)
		}
	}

	return nil
}

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
	// We rely on the writer to set feature bits correctly as those are
	// mostly static and the reader will also verify the features. This is
	// done to not having to pass in the known feature vector for writer
	// validation, similar to other write validation in this file.

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

// checkInvoiceNodeID enforces the spec rule that, when offer_issuer_id is
// present, invoice_node_id MUST equal it. Both fields live on the invoice, so
// this is verifiable without the originating offer. The offer_paths branch
// (invoice_node_id equals the final blinded_node_id on the arrival path) needs
// caller context and is not checked here. A present-but-nil offer_issuer_id or
// invoice_node_id is rejected separately as ErrNilPublicKey, so a nil here is
// treated as absent.
func checkInvoiceNodeID(inv *Invoice) error {
	// A present-but-nil offer_issuer_id is rejected separately as
	// ErrNilPublicKey, so a nil here means absent and there is nothing to
	// check.
	issuerID := inv.OfferIssuerID.ValOpt().UnwrapOr(nil)
	if issuerID == nil {
		return nil
	}

	// invoice_node_id is likewise guarded against present-but-nil by
	// checkPubKeyNotNil, so a nil here means absent; its required presence
	// is enforced separately as ErrMissingNodeID.
	nodeID := inv.InvoiceNodeID.ValOpt().UnwrapOr(nil)
	if nodeID == nil || !nodeID.IsEqual(issuerID) {
		return ErrInvoiceNodeIDMismatch
	}

	return nil
}

// ValidateInvoiceWrite validates an invoice per the BOLT 12 invoice writer
// requirements. The checks follow the spec's writer section in order.
// Requirements that depend on context this codec layer does not have
// (signing, the payment preimage, the offer or path the request arrived on)
// are noted inline as deferred to the caller or to a paired validator.
func ValidateInvoiceWrite(inv *Invoice) error {
	// - MUST set invoice_created_at to the number of seconds since Midnight
	//   1 January 1970, UTC when the invoice was created.
	if !inv.InvoiceCreatedAt.IsSome() {
		return ErrMissingCreatedAt
	}

	// - MUST set invoice_amount to the minimum amount it will accept, in
	//   units of the minimal lightning-payable unit (e.g. milli-satoshis
	//   for bitcoin) for invreq_chain.
	if !inv.InvoiceAmount.IsSome() {
		return ErrMissingAmount
	}

	// Policy extension: reject zero invoice_amount. The spec permits it
	// ("minimum amount it will accept"), but a zero-amount HTLC cannot
	// settle past the channel-layer dust limit. The typed
	// ErrZeroInvoiceAmount lets a spec-strict caller distinguish this from
	// a missing-field violation. Symmetric with ValidateInvoiceRead.
	if inv.InvoiceAmount.ValOpt().UnwrapOr(0) == 0 {
		return ErrZeroInvoiceAmount
	}

	// - if the invoice is in response to an invoice_request:
	//   - MUST copy all non-signature fields from the invoice request
	//     (including unknown fields).
	//   - if invreq_amount is present: MUST set invoice_amount to
	//     invreq_amount.
	//   - otherwise: MUST set invoice_amount to the expected amount.
	// NOT CHECKED HERE: the copy is performed by NewInvoiceFromRequest and
	// this validator runs on the assembled struct. The invoice_amount ==
	// invreq_amount equality and the byte-for-byte field mirror are
	// enforced when the invoice is paired with its request in
	// ValidateInvoiceAgainstRequest. The offer_currency "expected amount"
	// needs a live exchange rate the codec cannot compute.

	// - MUST set invoice_payment_hash to the SHA256 hash of the
	//   payment_preimage that will be given in return for payment.
	// NOT CHECKED HERE beyond presence: relating the hash to the preimage
	// needs the preimage, which lives with the caller's logic.
	if !inv.InvoicePaymentHash.IsSome() {
		return ErrMissingPaymentHash
	}

	// - if offer_issuer_id is present: MUST set invoice_node_id to
	//   offer_issuer_id.
	// - otherwise, if offer_paths is present: MUST set invoice_node_id to
	//   the final blinded_node_id on the path the request arrived on.
	// The offer_issuer_id case is enforced by checkInvoiceNodeID since both
	// fields live on the invoice. The offer_paths case needs the blinded
	// arrival path, which is caller context, so only presence is checked
	// for it.
	//
	// A present-but-nil pubkey passes IsSome but would panic the codec on
	// encode, so reject it before the presence check.
	if err := checkPubKeyNotNil(
		inv.InvoiceNodeID, "invoice_node_id",
	); err != nil {
		return err
	}
	if !inv.InvoiceNodeID.IsSome() {
		return ErrMissingNodeID
	}
	if err := checkInvoiceNodeID(inv); err != nil {
		return err
	}

	// - MUST specify exactly one signature TLV element: signature.
	//   - MUST set sig to the signature using invoice_node_id as described
	//     in Signature Calculation.
	// NOT CHECKED HERE: signing happens after this validator runs. The
	// string-codec layer rejects an unsigned invoice, mirroring
	// ValidateInvoiceRequestWrite.

	// - if the expiry for accepting payment is not 7200 seconds after
	//   invoice_created_at: MUST set invoice_relative_expiry.
	//   seconds_from_creation to the number of seconds after
	//   invoice_created_at that payment should not be attempted.
	// NOT CHECKED HERE: the writer chooses the expiry, so there is no rule
	// to enforce on the encoded value. The time comparison needs a clock
	// (see ValidateInvoiceExpiry).

	// - if it accepts onchain payments:
	//   - MAY specify invoice_fallbacks.
	//   - SHOULD specify invoice_fallbacks in order of most-preferred to
	//     least-preferred if it has a preference.
	//   - for the bitcoin chain, it MUST set each fallback_address with
	//     version as a valid witness version and address as a valid witness
	//     program.
	// NOT CHECKED HERE: the codec stays permissive so callers can inspect
	// raw fallbacks. The spec's ignore semantics are applied on the read
	// side by UsableFallbackAddresses.

	// - MUST include invoice_paths containing one or more paths to the
	//   node.
	//   - MUST specify invoice_paths in order of most-preferred to
	//     least-preferred if it has a preference.
	if !inv.InvoicePaths.IsSome() {
		return ErrMissingPaths
	}

	// Writer mirror of the reader rule rejecting a blinded_path with zero
	// hops.
	if err := checkBlindedPaths(inv.InvoicePaths); err != nil {
		return err
	}

	// - MUST include invoice_blindedpay with exactly one blinded_payinfo
	//   for each blinded_path in paths, in order.
	// - MUST set features in each blinded_payinfo to match
	//   encrypted_data_tlv.allowed_features (or empty, if no
	//   allowed_features).
	// NOT CHECKED HERE: matching each payinfo.features to its path's
	// encrypted_data_tlv allowed_features needs the decrypted path, which
	// is caller context. Only the 1:1 count is enforced below.
	bp, err := inv.InvoiceBlindedPay.ValOpt().UnwrapOrErr(
		ErrMissingBlindedPay,
	)
	if err != nil {
		return err
	}

	// invoice_paths presence is enforced above, so the default is never the
	// value used; UnwrapOr just avoids a second WhenSome.
	paths := inv.InvoicePaths.ValOpt().UnwrapOr(lnwire.BlindedPaths{})
	if len(paths.Paths) != len(bp.Infos) {
		return ErrBlindedPayMismatch
	}

	// A present-but-nil pubkey passes IsSome but would panic the codec on
	// encode, so reject the mirrored pubkey fields. Symmetric with
	// ValidateInvoiceRequestWrite.
	if err := fn.MapOptionZ(inv.InvreqPayerID.ValOpt(),
		func(pk *btcec.PublicKey) error {
			if pk == nil {
				return fmt.Errorf("%w: invreq_payer_id",
					ErrNilPublicKey)
			}

			return nil
		}); err != nil {
		return err
	}
	if err := fn.MapOptionZ(inv.OfferIssuerID.ValOpt(),
		func(pk *btcec.PublicKey) error {
			if pk == nil {
				return fmt.Errorf("%w: offer_issuer_id",
					ErrNilPublicKey)
			}

			return nil
		}); err != nil {
		return err
	}

	return nil
}

// defaultInvoiceRelativeExpiry is the spec-defined fallback when an invoice
// omits invoice_relative_expiry: two hours from creation.
const defaultInvoiceRelativeExpiry uint32 = 7200

// ValidateInvoiceExpiry rejects an invoice whose effective expiry is strictly
// before now. The effective expiry is invoice_created_at +
// invoice_relative_expiry, falling back to a 7200-second default per spec when
// relative expiry is absent. Per the BOLT 12 reader the invoice is rejected
// only when the current time is greater than the expiry, so the boundary second
// itself is still valid; this matches the strict comparison ValidateOfferRead
// uses for offer_absolute_expiry. Callers must invoke this separately after
// decoding. ValidateInvoiceRead covers the structural reader requirements, but
// the time check needs a clock the codec library doesn't supply.
func ValidateInvoiceExpiry(inv *Invoice, now time.Time) error {
	createdAt, err := inv.InvoiceCreatedAt.ValOpt().UnwrapOrErr(
		ErrMissingCreatedAt,
	)
	if err != nil {
		return err
	}

	relExpiry := inv.InvoiceRelativeExp.ValOpt().UnwrapOr(
		TUint32(defaultInvoiceRelativeExpiry),
	)

	// invoice_created_at + the relative expiry can overflow uint64 for an
	// absurd timestamp. The true sum then exceeds any real clock, so the
	// invoice is not expired: detect the carry rather than wrapping to a
	// small value that would spuriously read as expired.
	expiry, carry := bits.Add64(uint64(createdAt), uint64(relExpiry), 0)
	if carry == 0 && uint64(now.Unix()) > expiry {
		return ErrInvoiceExpired
	}

	return nil
}

// mirroredRecordBytes encodes the records in the invreq mirror range to their
// canonical per-record bytes, keyed by TLV type. This is the view the
// byte-for-byte invreq->invoice comparison operates on.
func mirroredRecordBytes(records []tlv.Record) (map[tlv.Type][]byte, error) {
	out := make(map[tlv.Type][]byte)
	for i := range records {
		r := records[i]
		if !invreqAllowedRange(r.Type()) {
			continue
		}
		buf, err := lnwire.EncodeRecords([]tlv.Record{r})
		if err != nil {
			return nil, fmt.Errorf(
				"encode record (type %d): %w", r.Type(), err,
			)
		}
		out[r.Type()] = buf
	}

	return out, nil
}

// ValidateInvoiceAgainstRequest performs a byte-for-byte comparison of the
// fields in ranges 0-159 and 1000000000-2999999999 between an invoice and its
// original request, as required by the BOLT 12 invoice reader specification.
// Callers must invoke this after pairing the invoice with its originating
// request. The codec library cannot reach across that pairing on its own.
//
// The comparison runs against the canonical per-record encoding from each
// side's AllRecords output. Two structs that decode to the same typed fields
// and the same ExtraSignedFields entries produce byte-identical encodings for
// any matching type. That is the byte-mirror invariant the spec demands.
//
// The amount cross-check enforces the spec's authorized-range rule: when
// invreq_amount is present, invoice_amount MUST equal it; otherwise the payer
// relied on the offer's fixed amount, so invoice_amount MUST be at least
// offer_amount * invreq_quantity for the native (bitcoin) case. The
// offer_currency case needs a caller-supplied exchange rate and is delegated to
// the caller.
func ValidateInvoiceAgainstRequest(inv *Invoice, req *InvoiceRequest) error {
	reqFields, err := mirroredRecordBytes(req.AllRecords())
	if err != nil {
		return fmt.Errorf("encode request fields: %w", err)
	}

	invFields, err := mirroredRecordBytes(inv.AllRecords())
	if err != nil {
		return fmt.Errorf("encode invoice fields: %w", err)
	}

	for typ, invBytes := range invFields {
		reqBytes, ok := reqFields[typ]
		if !ok {
			return fmt.Errorf("%w: invoice contains unexpected "+
				"field %d", ErrInvoiceMismatch, typ)
		}
		if !bytes.Equal(invBytes, reqBytes) {
			return fmt.Errorf("%w: field %d data mismatch",
				ErrInvoiceMismatch, typ)
		}
		delete(reqFields, typ)
	}

	if len(reqFields) > 0 {
		return fmt.Errorf("%w: invoice is missing %d fields from "+
			"request", ErrInvoiceMismatch, len(reqFields))
	}

	// Spec MUST: if invreq_amount (type 82) is present, invoice_amount
	// (type 170) must equal it. The byte-mirror loop cannot relate fields
	// with differing type numbers, so this cross-type equality is checked
	// explicitly.
	if req.InvreqAmount.IsSome() {
		invreqAmt := req.InvreqAmount.ValOpt().UnwrapOr(0)
		invAmt := inv.InvoiceAmount.ValOpt().UnwrapOr(0)
		if invAmt != invreqAmt {
			return fmt.Errorf("%w: invoice_amount %d != "+
				"invreq_amount %d", ErrInvoiceMismatch, invAmt,
				invreqAmt)
		}

		return nil
	}

	// Spec SHOULD: with invreq_amount absent the payer relied on the
	// offer's fixed amount, so confirm invoice_amount is within the
	// authorized range. For the native (non-offer_currency) case that range
	// is bounded below by offer_amount * invreq_quantity, computable here
	// from the mirrored offer fields. The offer_currency case is delegated
	// to the caller (see checkInvoiceAmountMeetsOffer).
	return checkInvoiceAmountMeetsOffer(inv)
}

// checkInvoiceAmountMeetsOffer confirms invoice_amount is at least the offer's
// authorized amount for the native (bitcoin) case, where the expected amount is
// offer_amount * invreq_quantity. It is a no-op when offer_amount is absent
// (there is nothing to bound against) or when offer_currency is present (the
// conversion into the invreq_chain currency needs a caller-supplied exchange
// rate, so the bound is delegated). This mirrors the request-side
// checkInvreqAmountMeetsOffer and is only meaningful when invreq_amount is
// absent, since a present invreq_amount pins invoice_amount by exact equality.
func checkInvoiceAmountMeetsOffer(inv *Invoice) error {
	if !inv.OfferAmount.IsSome() {
		return nil
	}

	// NOT CHECKED HERE: the offer_currency (non-bitcoin) case. Caller MUST
	// convert offer_amount to the invreq_chain currency and compare.
	if inv.OfferCurrency.IsSome() {
		return nil
	}

	offerAmt := uint64(inv.OfferAmount.ValOpt().UnwrapOr(0))
	qty := uint64(inv.InvreqQuantity.ValOpt().UnwrapOr(1))
	invAmt := uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0))

	// Guard against overflow of offer_amount * quantity.
	hi, expectedAmt := bits.Mul64(offerAmt, qty)
	if hi != 0 {
		return fmt.Errorf("%w: offer_amount %d * quantity %d "+
			"overflows uint64", ErrAmountBelowExpected, offerAmt,
			qty)
	}
	if invAmt < expectedAmt {
		return fmt.Errorf("%w: invoice_amount %d below expected %d",
			ErrAmountBelowExpected, invAmt, expectedAmt)
	}

	return nil
}

// isKnownInvoiceTLVType returns true for TLV types that are defined in the
// invoice spec.
func isKnownInvoiceTLVType(typ tlv.Type) bool {
	if isKnownInvreqTLVType(typ) {
		return true
	}

	switch typ {
	case invoicePathsType, invoiceBlindedPayType, invoiceCreatedAtType,
		invoiceRelativeExpiryType, invoicePaymentHashType,
		invoiceAmountType, invoiceFallbacksType, invoiceFeaturesType,
		invoiceNodeIDType:

		return true

	default:
		return false
	}
}

// InvoiceFeatureCatalogues names the two feature-bit catalogues the invoice
// reader validates against. They are grouped in a struct rather than passed as
// two positional map[lnwire.FeatureBit]string arguments because the identical
// types would otherwise let a caller transpose them silently: validating
// invoice_features against the blinded-path catalogue and vice versa compiles
// cleanly but misvalidates. Named fields make the swap impossible.
type InvoiceFeatureCatalogues struct {
	// Invoice names the feature bits the reader understands for the
	// top-level invoice_features field.
	Invoice map[lnwire.FeatureBit]string

	// Blinded names the feature bits the reader understands for each
	// blinded_payinfo.features field carried in invoice_blindedpay.
	Blinded map[lnwire.FeatureBit]string
}

// ValidateInvoiceRead validates an invoice against the BOLT 12 reader
// requirements, running the stateless structural checks against activeChain
// (the chain the reader supports).
//
// Note: This only performs stateless structural checks. Cryptographic Schnorr
// signature verification and identity-path binding are deferred to the caller
// (see the TODO at the end of this function). Additionally, while it verifies
// that at least one usable path is present, downstream callers must re-apply
// the same features.Blinded filter at path selection time (via
// Invoice.UsablePaths) to avoid selecting paths with unknown required features.
func ValidateInvoiceRead(inv *Invoice, activeChain [32]byte,
	features InvoiceFeatureCatalogues) error {
	// - MUST reject the invoice if invoice_amount is not present.
	if !inv.InvoiceAmount.IsSome() {
		return ErrMissingAmount
	}

	// Policy extension. See ValidateInvoiceWrite.
	if inv.InvoiceAmount.ValOpt().UnwrapOr(0) == 0 {
		return ErrZeroInvoiceAmount
	}

	// - MUST reject the invoice if invoice_created_at is not present.
	if !inv.InvoiceCreatedAt.IsSome() {
		return ErrMissingCreatedAt
	}

	// - MUST reject the invoice if invoice_payment_hash is not present.
	if !inv.InvoicePaymentHash.IsSome() {
		return ErrMissingPaymentHash
	}

	// - MUST reject the invoice if invoice_node_id is not present. A
	//   present-but-nil pubkey passes IsSome but would panic the codec, so
	//   reject it before the presence check.
	if err := checkPubKeyNotNil(
		inv.InvoiceNodeID, "invoice_node_id",
	); err != nil {
		return err
	}
	if !inv.InvoiceNodeID.IsSome() {
		return ErrMissingNodeID
	}

	// - if invreq_chain is not present:
	//   - MUST reject the invoice if bitcoin is not a supported chain.
	// - otherwise:
	//   - MUST reject the invoice if invreq_chain.chain is not a supported
	//     chain.
	// invreq_chain defaults to bitcoin mainnet when absent. activeChain is
	// the chain the reader supports.
	chain := inv.InvreqChain.ValOpt().UnwrapOr(bitcoinMainnetGenesisHash)
	if chain != activeChain {
		return ErrUnsupportedChain
	}

	// - if invoice_features contains unknown odd bits that are non-zero:
	//   - MUST ignore the bit.
	// - if invoice_features contains unknown even bits that are non-zero:
	//   - MUST reject the invoice.
	// checkFeatures enforces those invoice_features bit rules below.
	//
	// Separately, BOLT 1 makes unknown even TLV types must-understand, so
	// reject those here over the decoded type set. Unlike the
	// invoice_request reader, the invoice reader defines no out-of-range
	// type rejection, so unknown odd types are simply ignored ("it's ok to
	// be odd"). The signature range (240-1000) is exempt for the same
	// reason, matching the invoice_request reader and the Merkle path.
	for _, t := range sortedTypes(inv.decodedTLVs) {
		if bolt12InUnsignedRange(t) {
			continue
		}
		if !isKnownInvoiceTLVType(t) && t%2 == 0 {
			return fmt.Errorf("%w: type %d", ErrUnknownEvenType, t)
		}
	}
	err := checkFeatures(inv.InvoiceFeatures, features.Invoice)
	if err != nil {
		return err
	}

	// - if invoice_relative_expiry is present:
	//   - MUST reject the invoice if the current time since 1970-01-01 UTC
	//     is greater than invoice_created_at plus seconds_from_creation.
	// - otherwise:
	//   - MUST reject the invoice if the current time since 1970-01-01 UTC
	//     is greater than invoice_created_at plus 7200.
	// NOT CHECKED HERE: the comparison needs a clock the codec doesn't
	// supply. Callers run ValidateInvoiceExpiry separately.

	// - MUST reject the invoice if invoice_paths is not present or is
	//   empty.
	if !inv.InvoicePaths.IsSome() {
		return ErrMissingPaths
	}

	// - MUST reject the invoice if num_hops is 0 in any blinded_path in
	//   invoice_paths (checkBlindedPaths also rejects an empty path list).
	if err := checkBlindedPaths(inv.InvoicePaths); err != nil {
		return err
	}

	// - MUST reject the invoice if invoice_blindedpay is not present.
	bp, err := inv.InvoiceBlindedPay.ValOpt().UnwrapOrErr(
		ErrMissingBlindedPay,
	)
	if err != nil {
		return err
	}

	// - MUST reject the invoice if invoice_blindedpay does not contain
	//   exactly one blinded_payinfo per invoice_paths.blinded_path.
	paths := inv.InvoicePaths.ValOpt().UnwrapOr(lnwire.BlindedPaths{})
	if len(paths.Paths) != len(bp.Infos) {
		return ErrBlindedPayMismatch
	}

	// - For each invoice_blindedpay.payinfo:
	//   - MUST NOT use the corresponding invoice_paths.path if
	//     payinfo.features has any unknown even bits set.
	//   - MUST reject the invoice if this leaves no usable paths.
	// UsablePaths applies that filter; a caller selecting a path downstream
	// should use it rather than the unfiltered invoice_paths.
	if len(inv.UsablePaths(features.Blinded)) == 0 {
		return ErrNoUsablePaths
	}

	// - if the invoice is a response to an invoice_request:
	//   - MUST reject the invoice if all fields in ranges 0 to 159 and
	//     1000000000 to 2999999999 (inclusive) do not exactly match the
	//     invoice request.
	//   - if offer_issuer_id is present: MUST reject the invoice if
	//     invoice_node_id is not equal to offer_issuer_id.
	//   - otherwise, if offer_paths is present: MUST reject the invoice if
	//     invoice_node_id is not equal to the final blinded_node_id it sent
	//     the invoice request to.
	// The offer_issuer_id case is checked here by checkInvoiceNodeID (both
	// fields live on the invoice). NOT CHECKED HERE: the byte-for-byte
	// field mirror and the invreq_amount == invoice_amount rule are
	// enforced by ValidateInvoiceAgainstRequest once the invoice is paired
	// with its request; the offer_paths blinded_node_id case needs the
	// arrival path and stays with the caller.
	if err := checkInvoiceNodeID(inv); err != nil {
		return err
	}

	// - MUST reject the invoice if signature is not a valid signature using
	//   invoice_node_id as described in Signature Calculation.
	// TODO(bolt12): implement signature verification. For now only
	// presence is enforced, mirroring ValidateInvoiceRequestRead.
	if !inv.Signature.IsSome() {
		return ErrMissingSignature
	}

	// - SHOULD prefer to use earlier invoice_paths over later ones if it
	//   has no other reason for preference.
	// - if invoice_features contains the MPP/compulsory bit: MUST pay
	//   via multiple separate blinded paths; the MPP/optional bit MAY,
	//   otherwise MUST NOT use multiple parts.
	// - if invreq_amount is present: MUST reject the invoice if
	//   invoice_amount is not equal to invreq_amount (otherwise SHOULD
	//   confirm invoice_amount.msat is within the authorized range).
	// - for the bitcoin chain, if the invoice specifies invoice_fallbacks:
	//   - MUST ignore any fallback_address with version greater than 16,
	//   address shorter than 2 or longer than 40 bytes, or an address that
	//   does not meet known requirements for the given version.
	// - the invreq_paths / blinded-path / reply_path arrival rules.
	// NOT CHECKED HERE: these are payment-time or transport concerns
	// handled outside this codec. invreq_amount equality is enforced by
	// ValidateInvoiceAgainstRequest; the fallback ignore rules by
	// UsableFallbackAddresses.

	return nil
}
