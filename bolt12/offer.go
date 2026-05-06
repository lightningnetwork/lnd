package bolt12

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// Offer represents a BOLT 12 offer message. An offer is a long-lived, reusable
// payment template that can generate multiple invoices.
type Offer struct {
	// OfferChains specifies which chains this offer is valid for. If
	// absent, bitcoin is implied.
	OfferChains tlv.OptionalRecordT[tlv.TlvType2, ChainsRecord]

	// OfferMetadata is opaque data set by the offer creator for its own
	// use.
	OfferMetadata tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]

	// OfferCurrency is the ISO 4217 currency code for the offer amount, if
	// the amount is not in the chain's native unit.
	OfferCurrency tlv.OptionalRecordT[tlv.TlvType6, tlv.Blob]

	// OfferAmount is the amount expected per item, encoded as a tu64. The
	// unit depends on OfferCurrency (msat if absent).
	OfferAmount tlv.OptionalRecordT[tlv.TlvType8, TUint64]

	// OfferDescription is a UTF-8 description of the purpose of the
	// payment.
	OfferDescription tlv.OptionalRecordT[tlv.TlvType10, tlv.Blob]

	// OfferFeatures is the feature bit vector for this offer.
	OfferFeatures tlv.OptionalRecordT[tlv.TlvType12,
		lnwire.RawFeatureVector]

	// OfferAbsoluteExpiry is the time (seconds since epoch) after which the
	// offer should not be used, encoded as a tu64.
	OfferAbsoluteExpiry tlv.OptionalRecordT[tlv.TlvType14, TUint64]

	// OfferPaths contains one or more blinded paths to the offer issuer.
	OfferPaths tlv.OptionalRecordT[tlv.TlvType16, lnwire.BlindedPaths]

	// OfferIssuer is a UTF-8 string identifying the issuer.
	OfferIssuer tlv.OptionalRecordT[tlv.TlvType18, tlv.Blob]

	// OfferQuantityMax is the maximum number of items that can be requested
	// in a single invoice, encoded as a tu64. A value of 0 means unlimited.
	OfferQuantityMax tlv.OptionalRecordT[tlv.TlvType20, TUint64]

	// OfferIssuerID is the public key of the offer issuer. The codec
	// parses the 33-byte SEC1 compressed point on decode, so a struct
	// holding a key has already passed both the length and on-curve
	// checks.
	OfferIssuerID tlv.OptionalRecordT[tlv.TlvType22, *btcec.PublicKey]

	// decodedTLVs is the canonical TypeMap produced by decoding this offer.
	// Handled types map to nil; unhandled types map to their value bytes.
	// Encoding and validation both derive their view from this single field
	// so they cannot drift apart, and so signed-range extras the decoder
	// did not understand are re-emitted on encode and preserve offer_id.
	decodedTLVs tlv.TypeMap
}

var _ lnwire.PureTLVMessage = (*Offer)(nil)

// AllRecords returns the canonical sorted record list for this offer, merging
// the typed records with any extra signed-range fields that the decoder
// preserved.
func (o *Offer) AllRecords() []tlv.Record {
	return allRecordsFromTypeMap(
		o.allRecordProducers(), o.decodedTLVs,
	)
}

// allRecordProducers returns record producers for every set optional field, in
// declaration order.
func (o *Offer) allRecordProducers() []tlv.RecordProducer {
	var p []tlv.RecordProducer

	lnwire.AddOpt(&p, o.OfferChains)
	lnwire.AddOpt(&p, o.OfferMetadata)
	lnwire.AddOpt(&p, o.OfferCurrency)
	lnwire.AddOpt(&p, o.OfferAmount)
	lnwire.AddOpt(&p, o.OfferDescription)
	lnwire.AddOpt(&p, o.OfferFeatures)
	lnwire.AddOpt(&p, o.OfferAbsoluteExpiry)
	lnwire.AddOpt(&p, o.OfferPaths)
	lnwire.AddOpt(&p, o.OfferIssuer)
	lnwire.AddOpt(&p, o.OfferQuantityMax)
	lnwire.AddOpt(&p, o.OfferIssuerID)

	return p
}

// Encode serialises the offer into a canonical TLV byte stream.
func (o *Offer) Encode() ([]byte, error) {
	if err := ValidateOfferWrite(o); err != nil {
		return nil, fmt.Errorf("validate offer: %w", err)
	}

	var buf bytes.Buffer
	if err := lnwire.EncodePureTLVMessage(o, &buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeOffer parses a TLV byte stream into an Offer. Decoding is permissive —
// the spec writer requirements are not enforced here, so callers that need a
// valid offer must run ValidateOfferRead. Unknown TLVs are preserved on the
// returned offer so a later Encode can re-emit signed-range extras and keep
// offer_id stable.
func decodeOffer(data []byte) (*Offer, error) {
	var o Offer

	// Prepare zero-valued records for all optional fields so the TLV
	// decoder can populate them.
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

	tm, err := decodeStream(
		data,
		chains.Record(),
		metadata.Record(),
		currency.Record(),
		amount.Record(),
		desc.Record(),
		features.Record(),
		expiry.Record(),
		paths.Record(),
		issuer.Record(),
		qtyMax.Record(),
		issuerID.Record(),
	)
	if err != nil {
		return nil, fmt.Errorf("decode offer: %w", err)
	}

	lnwire.SetOptFromMap(tm, &o.OfferChains, chains)
	lnwire.SetOptFromMap(tm, &o.OfferMetadata, metadata)
	lnwire.SetOptFromMap(tm, &o.OfferCurrency, currency)
	lnwire.SetOptFromMap(tm, &o.OfferAmount, amount)
	lnwire.SetOptFromMap(tm, &o.OfferDescription, desc)
	lnwire.SetOptFromMap(tm, &o.OfferFeatures, features)
	lnwire.SetOptFromMap(tm, &o.OfferAbsoluteExpiry, expiry)
	lnwire.SetOptFromMap(tm, &o.OfferPaths, paths)
	lnwire.SetOptFromMap(tm, &o.OfferIssuer, issuer)
	lnwire.SetOptFromMap(tm, &o.OfferQuantityMax, qtyMax)
	lnwire.SetOptFromMap(tm, &o.OfferIssuerID, issuerID)

	o.decodedTLVs = tm

	return &o, nil
}
