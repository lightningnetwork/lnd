package bolt12

import (
	"bytes"
	"fmt"
	"maps"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// InvoiceRequest represents a BOLT 12 invoice_request message. It mirrors offer
// fields from the original offer. It also adds payer-specific fields and a
// Schnorr signature.
//
// An invoice request should be constructed from an offer (e.g., using
// NewInvoiceRequestFromOffer) unless it is a spontaneous invoice request.
type InvoiceRequest struct {
	// OfferChains are the chains that the mirrored offer is valid for.
	OfferChains tlv.OptionalRecordT[tlv.TlvType2, ChainsRecord]

	// OfferMetadata is the metadata from the mirrored offer.
	OfferMetadata tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]

	// OfferCurrency is the currency from the mirrored offer.
	OfferCurrency tlv.OptionalRecordT[tlv.TlvType6, tlv.Blob]

	// OfferAmount is the amount from the mirrored offer.
	OfferAmount tlv.OptionalRecordT[tlv.TlvType8, TUint64]

	// OfferDescription is the description from the mirrored offer.
	OfferDescription tlv.OptionalRecordT[tlv.TlvType10, tlv.Blob]

	// OfferFeatures are the features required by the mirrored offer.
	OfferFeatures tlv.OptionalRecordT[
		tlv.TlvType12, lnwire.RawFeatureVector,
	]

	// OfferAbsoluteExpiry is the absolute expiry from the mirrored offer.
	OfferAbsoluteExpiry tlv.OptionalRecordT[tlv.TlvType14, TUint64]

	// OfferPaths are the blinded paths from the mirrored offer.
	OfferPaths tlv.OptionalRecordT[tlv.TlvType16, lnwire.BlindedPaths]

	// OfferIssuer is the issuer name from the mirrored offer.
	OfferIssuer tlv.OptionalRecordT[tlv.TlvType18, tlv.Blob]

	// OfferQuantityMax is the maximum quantity allowed by the mirrored
	// offer.
	OfferQuantityMax tlv.OptionalRecordT[tlv.TlvType20, TUint64]

	// OfferIssuerID is the public key of the offer issuer.
	OfferIssuerID tlv.OptionalRecordT[tlv.TlvType22, *btcec.PublicKey]

	// InvreqMetadata is a blob of metadata provided by the payer.
	InvreqMetadata tlv.OptionalRecordT[tlv.TlvType0, tlv.Blob]

	// InvreqChain is the chain that the payer is using for this request.
	InvreqChain tlv.OptionalRecordT[tlv.TlvType80, [32]byte]

	// InvreqAmount is the amount the payer is offering to pay.
	InvreqAmount tlv.OptionalRecordT[tlv.TlvType82, TUint64]

	// InvreqFeatures are the features provided by the payer.
	InvreqFeatures tlv.OptionalRecordT[
		tlv.TlvType84, lnwire.RawFeatureVector,
	]

	// InvreqQuantity is the quantity of the offer item being requested.
	InvreqQuantity tlv.OptionalRecordT[tlv.TlvType86, TUint64]

	// InvreqPayerID is the public key the payer uses to sign the request.
	InvreqPayerID tlv.OptionalRecordT[tlv.TlvType88, *btcec.PublicKey]

	// InvreqPayerNote is an optional note from the payer.
	InvreqPayerNote tlv.OptionalRecordT[tlv.TlvType89, tlv.Blob]

	// InvreqPaths are the blinded paths the payer wants the invoice to be
	// sent to.
	InvreqPaths tlv.OptionalRecordT[tlv.TlvType90, lnwire.BlindedPaths]

	// InvreqBip353Name is the BIP 353 name of the payer.
	InvreqBip353Name tlv.OptionalRecordT[tlv.TlvType91, tlv.Blob]

	// Signature is a BIP-340 Schnorr signature covering all fields.
	Signature tlv.OptionalRecordT[tlv.TlvType240, [64]byte]

	// decodedTLVs is the canonical TypeMap produced by the typed- stream
	// pass that decoded this request. See Offer.decodedTLVs for the design
	// rationale.
	decodedTLVs tlv.TypeMap
}

// AllRecords returns the canonical sorted record list for this invoice request,
// merging the typed records with any extra signed-range fields that the decoder
// preserved.
//
// NOTE: this is part of the tlv.PureTLVMessage interface.
func (ir *InvoiceRequest) AllRecords() []tlv.Record {
	return allRecordsFromTypeMap(
		ir.allRecordProducers(), ir.decodedTLVs,
	)
}

var _ lnwire.PureTLVMessage = (*InvoiceRequest)(nil)

// allRecordProducers returns the set of records that are present.
func (ir *InvoiceRequest) allRecordProducers() []tlv.RecordProducer {
	var p []tlv.RecordProducer

	lnwire.AddOpt(&p, ir.InvreqMetadata)
	lnwire.AddOpt(&p, ir.OfferChains)
	lnwire.AddOpt(&p, ir.OfferMetadata)
	lnwire.AddOpt(&p, ir.OfferCurrency)
	lnwire.AddOpt(&p, ir.OfferAmount)
	lnwire.AddOpt(&p, ir.OfferDescription)
	lnwire.AddOpt(&p, ir.OfferFeatures)
	lnwire.AddOpt(&p, ir.OfferAbsoluteExpiry)
	lnwire.AddOpt(&p, ir.OfferPaths)
	lnwire.AddOpt(&p, ir.OfferIssuer)
	lnwire.AddOpt(&p, ir.OfferQuantityMax)
	lnwire.AddOpt(&p, ir.OfferIssuerID)
	lnwire.AddOpt(&p, ir.InvreqChain)
	lnwire.AddOpt(&p, ir.InvreqAmount)
	lnwire.AddOpt(&p, ir.InvreqFeatures)
	lnwire.AddOpt(&p, ir.InvreqQuantity)
	lnwire.AddOpt(&p, ir.InvreqPayerID)
	lnwire.AddOpt(&p, ir.InvreqPayerNote)
	lnwire.AddOpt(&p, ir.InvreqPaths)
	lnwire.AddOpt(&p, ir.InvreqBip353Name)
	lnwire.AddOpt(&p, ir.Signature)

	return p
}

// Encode serialises the invoice request via the PureTLVMessage shape.
// The per-record canonicalisation is pure: a struct mutated and
// re-encoded reflects the new bytes without any sidecar rehydration
// step.
func (ir *InvoiceRequest) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := lnwire.EncodePureTLVMessage(ir, &buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeInvoiceRequest deserializes an invoice request from a TLV byte stream.
// Decoding is permissive: callers that need spec compliance must run
// ValidateInvoiceRequestRead.
func DecodeInvoiceRequest(data []byte) (*InvoiceRequest, error) {
	var ir InvoiceRequest

	invreqMetadata := tlv.ZeroRecordT[tlv.TlvType0, tlv.Blob]()
	chains := tlv.ZeroRecordT[tlv.TlvType2, ChainsRecord]()
	metadata := tlv.ZeroRecordT[tlv.TlvType4, tlv.Blob]()
	currency := tlv.ZeroRecordT[tlv.TlvType6, tlv.Blob]()
	amount := tlv.ZeroRecordT[tlv.TlvType8, TUint64]()
	desc := tlv.ZeroRecordT[tlv.TlvType10, tlv.Blob]()
	features := tlv.ZeroRecordT[tlv.TlvType12, lnwire.RawFeatureVector]()
	expiry := tlv.ZeroRecordT[tlv.TlvType14, TUint64]()
	paths := tlv.ZeroRecordT[tlv.TlvType16, lnwire.BlindedPaths]()
	issuer := tlv.ZeroRecordT[tlv.TlvType18, tlv.Blob]()
	qtyMax := tlv.ZeroRecordT[tlv.TlvType20, TUint64]()
	issuerID := tlv.ZeroRecordT[tlv.TlvType22, *btcec.PublicKey]()
	invreqChain := tlv.ZeroRecordT[tlv.TlvType80, [32]byte]()
	invreqAmount := tlv.ZeroRecordT[tlv.TlvType82, TUint64]()
	invreqFeatures := tlv.ZeroRecordT[
		tlv.TlvType84, lnwire.RawFeatureVector,
	]()
	invreqQty := tlv.ZeroRecordT[tlv.TlvType86, TUint64]()
	payerID := tlv.ZeroRecordT[tlv.TlvType88, *btcec.PublicKey]()
	payerNote := tlv.ZeroRecordT[tlv.TlvType89, tlv.Blob]()
	invreqPaths := tlv.ZeroRecordT[tlv.TlvType90, lnwire.BlindedPaths]()
	bip353 := tlv.ZeroRecordT[tlv.TlvType91, tlv.Blob]()
	sig := tlv.ZeroRecordT[tlv.TlvType240, [64]byte]()

	tm, err := decodeStream(
		data, invreqMetadata.Record(), chains.Record(),
		metadata.Record(), currency.Record(), amount.Record(),
		desc.Record(), features.Record(), expiry.Record(),
		paths.Record(), issuer.Record(), qtyMax.Record(),
		issuerID.Record(), invreqChain.Record(), invreqAmount.Record(),
		invreqFeatures.Record(), invreqQty.Record(), payerID.Record(),
		payerNote.Record(), invreqPaths.Record(), bip353.Record(),
		sig.Record(),
	)
	if err != nil {
		return nil, fmt.Errorf("decode invoice request: %w", err)
	}

	lnwire.SetOptFromMap(tm, &ir.InvreqMetadata, invreqMetadata)
	lnwire.SetOptFromMap(tm, &ir.OfferChains, chains)
	lnwire.SetOptFromMap(tm, &ir.OfferMetadata, metadata)
	lnwire.SetOptFromMap(tm, &ir.OfferCurrency, currency)
	lnwire.SetOptFromMap(tm, &ir.OfferAmount, amount)
	lnwire.SetOptFromMap(tm, &ir.OfferDescription, desc)
	lnwire.SetOptFromMap(tm, &ir.OfferFeatures, features)
	lnwire.SetOptFromMap(tm, &ir.OfferAbsoluteExpiry, expiry)
	lnwire.SetOptFromMap(tm, &ir.OfferPaths, paths)
	lnwire.SetOptFromMap(tm, &ir.OfferIssuer, issuer)
	lnwire.SetOptFromMap(tm, &ir.OfferQuantityMax, qtyMax)
	lnwire.SetOptFromMap(tm, &ir.OfferIssuerID, issuerID)
	lnwire.SetOptFromMap(tm, &ir.InvreqChain, invreqChain)
	lnwire.SetOptFromMap(tm, &ir.InvreqAmount, invreqAmount)
	lnwire.SetOptFromMap(tm, &ir.InvreqFeatures, invreqFeatures)
	lnwire.SetOptFromMap(tm, &ir.InvreqQuantity, invreqQty)
	lnwire.SetOptFromMap(tm, &ir.InvreqPayerID, payerID)
	lnwire.SetOptFromMap(tm, &ir.InvreqPayerNote, payerNote)
	lnwire.SetOptFromMap(tm, &ir.InvreqPaths, invreqPaths)
	lnwire.SetOptFromMap(tm, &ir.InvreqBip353Name, bip353)
	lnwire.SetOptFromMap(tm, &ir.Signature, sig)

	ir.decodedTLVs = tm

	return &ir, nil
}

// NewInvoiceRequestFromOffer constructs a new InvoiceRequest by copying
// (mirroring) all fields from the provided Offer. It assigns the payer ID and
// payer metadata; the caller should subsequently sign the request.
//
// Per "MUST copy all fields from the offer (including unknown fields)", the
// offer's unknown TLVs are carried via the decodedTLVs sidecar so they are
// signed and mirrored into the invoice.
//
// chain is the genesis hash the payer intends to pay on. invreq_chain is set
// only when chain is not Bitcoin mainnet (absent defaults to mainnet); writer
// validation enforces that it is one of the offer's chains.
func NewInvoiceRequestFromOffer(offer *Offer, payerID *btcec.PublicKey,
	metadata []byte, chain [32]byte) *InvoiceRequest {

	ir := &InvoiceRequest{
		OfferChains:         offer.OfferChains,
		OfferMetadata:       offer.OfferMetadata,
		OfferCurrency:       offer.OfferCurrency,
		OfferAmount:         offer.OfferAmount,
		OfferDescription:    offer.OfferDescription,
		OfferFeatures:       offer.OfferFeatures,
		OfferAbsoluteExpiry: offer.OfferAbsoluteExpiry,
		OfferPaths:          offer.OfferPaths,
		OfferIssuer:         offer.OfferIssuer,
		OfferQuantityMax:    offer.OfferQuantityMax,
		OfferIssuerID:       offer.OfferIssuerID,

		// Carry the offer's unknown signed-range TLVs. Known offer
		// types appear in the map with nil values and are skipped when
		// the sidecar is merged, so this re-emits only the unknowns and
		// never duplicates the typed fields copied above.
		decodedTLVs: maps.Clone(offer.decodedTLVs),
	}

	// Set invreq_chain only for non-bitcoin chains; for bitcoin mainnet the
	// spec says SHOULD omit, and an absent invreq_chain defaults back to
	// mainnet on the read side.
	if chain != bitcoinMainnetGenesisHash {
		ir.InvreqChain = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80, [32]byte](chain),
		)
	}

	if len(metadata) > 0 {
		ir.InvreqMetadata = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType0, tlv.Blob]{
				Val: metadata,
			},
		)
	}

	if payerID != nil {
		ir.InvreqPayerID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](payerID),
		)
	}

	return ir
}
