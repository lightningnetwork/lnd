package bolt12

import (
	"bytes"
	"fmt"
	"maps"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// Invoice represents a BOLT 12 invoice message. It mirrors all non-signature
// invoice_request fields (types 0-91) and adds invoice-specific fields (types
// 160-176) plus a Schnorr signature (type 240).
//
// An invoice in response to a request should be constructed from that request
// (e.g., using NewInvoiceFromRequest) to mirror its fields. The caller then
// populates the invoice-specific fields and signs it.
type Invoice struct {
	// Fields in the 0-91 range are mirrored verbatim from the
	// invoice_request (which carries the offer's fields); the byte-for-byte
	// match is enforced by ValidateInvoiceAgainstRequest.

	// InvreqMetadata is the payer metadata.
	InvreqMetadata tlv.OptionalRecordT[tlv.TlvType0, tlv.Blob]

	// OfferChains are the chains the offer is valid for.
	OfferChains tlv.OptionalRecordT[tlv.TlvType2, ChainsRecord]

	// OfferMetadata is the offer metadata.
	OfferMetadata tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]

	// OfferCurrency is the offer currency.
	OfferCurrency tlv.OptionalRecordT[tlv.TlvType6, tlv.Blob]

	// OfferAmount is the offer amount.
	OfferAmount tlv.OptionalRecordT[tlv.TlvType8, TUint64]

	// OfferDescription is the offer description.
	OfferDescription tlv.OptionalRecordT[tlv.TlvType10, tlv.Blob]

	// OfferFeatures are the offer features.
	OfferFeatures tlv.OptionalRecordT[
		tlv.TlvType12, lnwire.RawFeatureVector,
	]

	// OfferAbsoluteExpiry is the offer's absolute expiry.
	OfferAbsoluteExpiry tlv.OptionalRecordT[tlv.TlvType14, TUint64]

	// OfferPaths are the offer's blinded paths.
	OfferPaths tlv.OptionalRecordT[tlv.TlvType16, lnwire.BlindedPaths]

	// OfferIssuer is the offer issuer name.
	OfferIssuer tlv.OptionalRecordT[tlv.TlvType18, tlv.Blob]

	// OfferQuantityMax is the offer's maximum quantity.
	OfferQuantityMax tlv.OptionalRecordT[tlv.TlvType20, TUint64]

	// OfferIssuerID is the offer issuer's public key.
	OfferIssuerID tlv.OptionalRecordT[tlv.TlvType22, *btcec.PublicKey]

	// InvreqChain is the requested chain.
	InvreqChain tlv.OptionalRecordT[tlv.TlvType80, [32]byte]

	// InvreqAmount is the amount the payer offered.
	InvreqAmount tlv.OptionalRecordT[tlv.TlvType82, TUint64]

	// InvreqFeatures are the payer's features.
	InvreqFeatures tlv.OptionalRecordT[
		tlv.TlvType84, lnwire.RawFeatureVector,
	]

	// InvreqQuantity is the requested quantity.
	InvreqQuantity tlv.OptionalRecordT[tlv.TlvType86, TUint64]

	// InvreqPayerID is the payer's signing public key.
	InvreqPayerID tlv.OptionalRecordT[tlv.TlvType88, *btcec.PublicKey]

	// InvreqPayerNote is an optional payer note.
	InvreqPayerNote tlv.OptionalRecordT[tlv.TlvType89, tlv.Blob]

	// InvreqPaths are the payer's blinded paths to send the invoice to.
	InvreqPaths tlv.OptionalRecordT[tlv.TlvType90, lnwire.BlindedPaths]

	// InvreqBip353Name is the payer's BIP 353 name.
	InvreqBip353Name tlv.OptionalRecordT[tlv.TlvType91, tlv.Blob]

	// Fields from type 160 on are invoice-specific.

	// InvoicePaths are the blinded paths to the recipient node.
	InvoicePaths tlv.OptionalRecordT[tlv.TlvType160, lnwire.BlindedPaths]

	// InvoiceBlindedPay carries one blinded_payinfo per invoice_paths
	// entry, in order.
	InvoiceBlindedPay tlv.OptionalRecordT[tlv.TlvType162, BlindedPayInfos]

	// InvoiceCreatedAt is the creation time in seconds since the Unix
	// epoch.
	InvoiceCreatedAt tlv.OptionalRecordT[tlv.TlvType164, TUint64]

	// InvoiceRelativeExp is the expiry in seconds after creation. When
	// absent the spec default of 7200 seconds applies.
	InvoiceRelativeExp tlv.OptionalRecordT[tlv.TlvType166, TUint32]

	// InvoicePaymentHash is the SHA256 hash of the payment preimage.
	InvoicePaymentHash tlv.OptionalRecordT[tlv.TlvType168, [32]byte]

	// InvoiceAmount is the minimum amount the payee will accept, in the
	// minimal payable unit of invreq_chain.
	InvoiceAmount tlv.OptionalRecordT[tlv.TlvType170, TUint64]

	// InvoiceFallbacks are optional on-chain fallback addresses.
	InvoiceFallbacks tlv.OptionalRecordT[
		tlv.TlvType172, FallbackAddresses,
	]

	// InvoiceFeatures are the features of the invoice.
	InvoiceFeatures tlv.OptionalRecordT[
		tlv.TlvType174, lnwire.RawFeatureVector,
	]

	// InvoiceNodeID is the public key of the recipient node, used to verify
	// the signature.
	InvoiceNodeID tlv.OptionalRecordT[tlv.TlvType176, *btcec.PublicKey]

	// Signature is a BIP-340 Schnorr signature covering all fields.
	Signature tlv.OptionalRecordT[tlv.TlvType240, [64]byte]

	// decodedTLVs is the canonical TypeMap produced by the typed-stream
	// pass that decoded this invoice. See Offer.decodedTLVs for the design
	// rationale.
	decodedTLVs tlv.TypeMap
}

// AllRecords returns the canonical sorted record list for this invoice, merging
// the typed records with any extra signed-range fields that the decoder
// preserved.
//
// NOTE: this is part of the tlv.PureTLVMessage interface.
func (inv *Invoice) AllRecords() []tlv.Record {
	return allRecordsFromTypeMap(
		inv.allRecordProducers(), inv.decodedTLVs,
	)
}

var _ lnwire.PureTLVMessage = (*Invoice)(nil)

const (
	// maxWitnessVersion is the highest segwit witness version a usable
	// fallback address may carry; the BOLT 12 reader ignores anything
	// above it.
	maxWitnessVersion = 16

	// minWitnessProgramLen and maxWitnessProgramLen bound the witness
	// program length, in bytes, of a usable fallback address.
	minWitnessProgramLen = 2
	maxWitnessProgramLen = 40
)

// UsableFallbackAddresses returns the invoice_fallbacks entries a payer may use
// after applying the BOLT 12 reader's MUST-ignore rules for the bitcoin chain.
func (inv *Invoice) UsableFallbackAddresses() []FallbackAddress {
	// Unwrap the optional up front so the filtering loop stays flat; a nil
	// Addrs slice ranges as empty.
	fallbacks := inv.InvoiceFallbacks.ValOpt().UnwrapOr(FallbackAddresses{})

	var addrs []FallbackAddress
	for _, a := range fallbacks.Addrs {
		// MUST ignore any fallback_address for which version is greater
		// than 16.
		if a.Version > maxWitnessVersion {
			continue
		}

		// MUST ignore any fallback_address for which address is less
		// than 2 or greater than 40 bytes.
		if len(a.Address) < minWitnessProgramLen ||
			len(a.Address) > maxWitnessProgramLen {

			continue
		}

		// MUST ignore any fallback_address for which address does not
		// meet known requirements for the given version. NOT enforced
		// here: the per-version witness-program check needs on-chain
		// address rules above this codec, so a caller dispatching
		// on-chain MUST apply it.
		addrs = append(addrs, a)
	}

	return addrs
}

// UsablePath pairs a blinded path with its payment parameters, as returned by
// UsablePaths after the BOLT 12 reader's feature filter has been applied.
type UsablePath struct {
	// Path is the blinded path to the recipient.
	Path lnwire.BlindedPath

	// PayInfo is the blinded_payinfo for Path.
	PayInfo BlindedPayInfo
}

// UsablePaths returns the invoice_paths entries a payer may use, each paired
// with its blinded_payinfo, after applying the BOLT 12 reader rule that a path
// MUST NOT be used when its payinfo.features has unknown required (even) bits
// set. knownBlindedFeatures names the feature bits the reader understands.
//
// The result is empty when invoice_paths or invoice_blindedpay is absent, or
// when the two lists differ in length; ValidateInvoiceRead rejects those cases
// separately, so a caller that validates first can treat an empty result as
// "no usable paths".
func (inv *Invoice) UsablePaths(
	knownBlindedFeatures map[lnwire.FeatureBit]string) []UsablePath {

	paths := inv.InvoicePaths.ValOpt().UnwrapOr(lnwire.BlindedPaths{})
	bp := inv.InvoiceBlindedPay.ValOpt().UnwrapOr(BlindedPayInfos{})

	// Entries pair by index; a length mismatch is rejected upstream by
	// ValidateInvoiceRead, so guard here to stay in bounds.
	if len(paths.Paths) != len(bp.Infos) {
		return nil
	}

	var usable []UsablePath
	for i := range bp.Infos {
		// MUST NOT use the path if payinfo.features has any unknown
		// even bits set.
		fv := bp.Infos[i].Features
		wrapped := lnwire.NewFeatureVector(&fv, knownBlindedFeatures)
		if len(wrapped.UnknownRequiredFeatures()) > 0 {
			continue
		}

		usable = append(usable, UsablePath{
			Path:    paths.Paths[i],
			PayInfo: bp.Infos[i],
		})
	}

	return usable
}

// allRecordProducers returns record producers for all set fields.
func (inv *Invoice) allRecordProducers() []tlv.RecordProducer {
	var p []tlv.RecordProducer

	// Invreq mirrored fields.
	lnwire.AddOpt(&p, inv.InvreqMetadata)
	lnwire.AddOpt(&p, inv.OfferChains)
	lnwire.AddOpt(&p, inv.OfferMetadata)
	lnwire.AddOpt(&p, inv.OfferCurrency)
	lnwire.AddOpt(&p, inv.OfferAmount)
	lnwire.AddOpt(&p, inv.OfferDescription)
	lnwire.AddOpt(&p, inv.OfferFeatures)
	lnwire.AddOpt(&p, inv.OfferAbsoluteExpiry)
	lnwire.AddOpt(&p, inv.OfferPaths)
	lnwire.AddOpt(&p, inv.OfferIssuer)
	lnwire.AddOpt(&p, inv.OfferQuantityMax)
	lnwire.AddOpt(&p, inv.OfferIssuerID)
	lnwire.AddOpt(&p, inv.InvreqChain)
	lnwire.AddOpt(&p, inv.InvreqAmount)
	lnwire.AddOpt(&p, inv.InvreqFeatures)
	lnwire.AddOpt(&p, inv.InvreqQuantity)
	lnwire.AddOpt(&p, inv.InvreqPayerID)
	lnwire.AddOpt(&p, inv.InvreqPayerNote)
	lnwire.AddOpt(&p, inv.InvreqPaths)
	lnwire.AddOpt(&p, inv.InvreqBip353Name)

	// Invoice-specific fields.
	lnwire.AddOpt(&p, inv.InvoicePaths)
	lnwire.AddOpt(&p, inv.InvoiceBlindedPay)
	lnwire.AddOpt(&p, inv.InvoiceCreatedAt)
	lnwire.AddOpt(&p, inv.InvoiceRelativeExp)
	lnwire.AddOpt(&p, inv.InvoicePaymentHash)
	lnwire.AddOpt(&p, inv.InvoiceAmount)
	lnwire.AddOpt(&p, inv.InvoiceFallbacks)
	lnwire.AddOpt(&p, inv.InvoiceFeatures)
	lnwire.AddOpt(&p, inv.InvoiceNodeID)
	lnwire.AddOpt(&p, inv.Signature)

	return p
}

// Encode validates the invoice per writer requirements and serialises it via
// the PureTLVMessage shape.
func (inv *Invoice) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := lnwire.EncodePureTLVMessage(inv, &buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeInvoice deserializes an invoice from a TLV byte stream. Decoding is
// permissive: callers that need spec compliance must run ValidateInvoiceRead.
func DecodeInvoice(data []byte) (*Invoice, error) {
	var inv Invoice

	invreqMetadata := tlv.ZeroRecordT[tlv.TlvType0, tlv.Blob]()
	chains := tlv.ZeroRecordT[tlv.TlvType2, ChainsRecord]()
	offerMeta := tlv.ZeroRecordT[tlv.TlvType4, tlv.Blob]()
	currency := tlv.ZeroRecordT[tlv.TlvType6, tlv.Blob]()
	offerAmt := tlv.ZeroRecordT[tlv.TlvType8, TUint64]()
	desc := tlv.ZeroRecordT[tlv.TlvType10, tlv.Blob]()
	offerFeat := tlv.ZeroRecordT[tlv.TlvType12, lnwire.RawFeatureVector]()
	expiry := tlv.ZeroRecordT[tlv.TlvType14, TUint64]()
	offerPaths := tlv.ZeroRecordT[tlv.TlvType16, lnwire.BlindedPaths]()
	issuer := tlv.ZeroRecordT[tlv.TlvType18, tlv.Blob]()
	qtyMax := tlv.ZeroRecordT[tlv.TlvType20, TUint64]()
	issuerID := tlv.ZeroRecordT[tlv.TlvType22, *btcec.PublicKey]()
	invreqChain := tlv.ZeroRecordT[tlv.TlvType80, [32]byte]()
	invreqAmt := tlv.ZeroRecordT[tlv.TlvType82, TUint64]()
	invreqFeat := tlv.ZeroRecordT[tlv.TlvType84, lnwire.RawFeatureVector]()
	invreqQty := tlv.ZeroRecordT[tlv.TlvType86, TUint64]()
	payerID := tlv.ZeroRecordT[tlv.TlvType88, *btcec.PublicKey]()
	payerNote := tlv.ZeroRecordT[tlv.TlvType89, tlv.Blob]()
	invreqPaths := tlv.ZeroRecordT[tlv.TlvType90, lnwire.BlindedPaths]()
	bip353 := tlv.ZeroRecordT[tlv.TlvType91, tlv.Blob]()
	invPaths := tlv.ZeroRecordT[tlv.TlvType160, lnwire.BlindedPaths]()
	blindedPay := tlv.ZeroRecordT[tlv.TlvType162, BlindedPayInfos]()
	createdAt := tlv.ZeroRecordT[tlv.TlvType164, TUint64]()
	relExp := tlv.ZeroRecordT[tlv.TlvType166, TUint32]()
	payHash := tlv.ZeroRecordT[tlv.TlvType168, [32]byte]()
	invAmt := tlv.ZeroRecordT[tlv.TlvType170, TUint64]()
	fallbacks := tlv.ZeroRecordT[tlv.TlvType172, FallbackAddresses]()
	invFeat := tlv.ZeroRecordT[tlv.TlvType174, lnwire.RawFeatureVector]()
	nodeID := tlv.ZeroRecordT[tlv.TlvType176, *btcec.PublicKey]()
	sig := tlv.ZeroRecordT[tlv.TlvType240, [64]byte]()

	tm, err := decodeStream(
		data,
		invreqMetadata.Record(), chains.Record(), offerMeta.Record(),
		currency.Record(), offerAmt.Record(), desc.Record(),
		offerFeat.Record(), expiry.Record(), offerPaths.Record(),
		issuer.Record(), qtyMax.Record(), issuerID.Record(),
		invreqChain.Record(), invreqAmt.Record(), invreqFeat.Record(),
		invreqQty.Record(), payerID.Record(), payerNote.Record(),
		invreqPaths.Record(), bip353.Record(), invPaths.Record(),
		blindedPay.Record(), createdAt.Record(), relExp.Record(),
		payHash.Record(), invAmt.Record(), fallbacks.Record(),
		invFeat.Record(), nodeID.Record(), sig.Record(),
	)
	if err != nil {
		return nil, fmt.Errorf("decode invoice: %w", err)
	}

	lnwire.SetOptFromMap(tm, &inv.InvreqMetadata, invreqMetadata)
	lnwire.SetOptFromMap(tm, &inv.OfferChains, chains)
	lnwire.SetOptFromMap(tm, &inv.OfferMetadata, offerMeta)
	lnwire.SetOptFromMap(tm, &inv.OfferCurrency, currency)
	lnwire.SetOptFromMap(tm, &inv.OfferAmount, offerAmt)
	lnwire.SetOptFromMap(tm, &inv.OfferDescription, desc)
	lnwire.SetOptFromMap(tm, &inv.OfferFeatures, offerFeat)
	lnwire.SetOptFromMap(tm, &inv.OfferAbsoluteExpiry, expiry)
	lnwire.SetOptFromMap(tm, &inv.OfferPaths, offerPaths)
	lnwire.SetOptFromMap(tm, &inv.OfferIssuer, issuer)
	lnwire.SetOptFromMap(tm, &inv.OfferQuantityMax, qtyMax)
	lnwire.SetOptFromMap(tm, &inv.OfferIssuerID, issuerID)
	lnwire.SetOptFromMap(tm, &inv.InvreqChain, invreqChain)
	lnwire.SetOptFromMap(tm, &inv.InvreqAmount, invreqAmt)
	lnwire.SetOptFromMap(tm, &inv.InvreqFeatures, invreqFeat)
	lnwire.SetOptFromMap(tm, &inv.InvreqQuantity, invreqQty)
	lnwire.SetOptFromMap(tm, &inv.InvreqPayerID, payerID)
	lnwire.SetOptFromMap(tm, &inv.InvreqPayerNote, payerNote)
	lnwire.SetOptFromMap(tm, &inv.InvreqPaths, invreqPaths)
	lnwire.SetOptFromMap(tm, &inv.InvreqBip353Name, bip353)
	lnwire.SetOptFromMap(tm, &inv.InvoicePaths, invPaths)
	lnwire.SetOptFromMap(tm, &inv.InvoiceBlindedPay, blindedPay)
	lnwire.SetOptFromMap(tm, &inv.InvoiceCreatedAt, createdAt)
	lnwire.SetOptFromMap(tm, &inv.InvoiceRelativeExp, relExp)
	lnwire.SetOptFromMap(tm, &inv.InvoicePaymentHash, payHash)
	lnwire.SetOptFromMap(tm, &inv.InvoiceAmount, invAmt)
	lnwire.SetOptFromMap(tm, &inv.InvoiceFallbacks, fallbacks)
	lnwire.SetOptFromMap(tm, &inv.InvoiceFeatures, invFeat)
	lnwire.SetOptFromMap(tm, &inv.InvoiceNodeID, nodeID)
	lnwire.SetOptFromMap(tm, &inv.Signature, sig)

	inv.decodedTLVs = tm

	return &inv, nil
}

// NewInvoiceFromRequest constructs a new Invoice by copying (mirroring) all
// non-signature fields from the provided InvoiceRequest. When invreq_amount is
// present it is mirrored into invoice_amount per the writer requirement. The
// caller is responsible for populating the remaining invoice-specific fields
// (invoice_created_at, invoice_payment_hash, invoice_node_id, invoice_paths,
// invoice_blindedpay, ...) and signing the invoice.
func NewInvoiceFromRequest(req *InvoiceRequest) *Invoice {
	inv := &Invoice{
		InvreqMetadata:      req.InvreqMetadata,
		OfferChains:         req.OfferChains,
		OfferMetadata:       req.OfferMetadata,
		OfferCurrency:       req.OfferCurrency,
		OfferAmount:         req.OfferAmount,
		OfferDescription:    req.OfferDescription,
		OfferFeatures:       req.OfferFeatures,
		OfferAbsoluteExpiry: req.OfferAbsoluteExpiry,
		OfferPaths:          req.OfferPaths,
		OfferIssuer:         req.OfferIssuer,
		OfferQuantityMax:    req.OfferQuantityMax,
		OfferIssuerID:       req.OfferIssuerID,
		InvreqChain:         req.InvreqChain,
		InvreqAmount:        req.InvreqAmount,
		InvreqFeatures:      req.InvreqFeatures,
		InvreqQuantity:      req.InvreqQuantity,
		InvreqPayerID:       req.InvreqPayerID,
		InvreqPayerNote:     req.InvreqPayerNote,
		InvreqPaths:         req.InvreqPaths,
		InvreqBip353Name:    req.InvreqBip353Name,

		// Carry the request's unknown signed-range TLVs. Known invreq
		// types appear in the map with nil values and are skipped when
		// the sidecar is merged, so this re-emits only the unknowns and
		// never duplicates the typed fields copied above. Any
		// signature-range entries (240-1000) cloned here are inert:
		// allRecordsFromTypeMap drops them via bolt12InUnsignedRange,
		// so the request's signature never leaks into the invoice.
		decodedTLVs: maps.Clone(req.decodedTLVs),
	}

	// Writer rule: if invreq_amount is present, invoice_amount MUST be set
	// to it. When absent, the caller sets the expected amount.
	req.InvreqAmount.WhenSome(
		func(r tlv.RecordT[tlv.TlvType82, TUint64]) {
			inv.InvoiceAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType170, TUint64](r.Val),
			)
		},
	)

	return inv
}
