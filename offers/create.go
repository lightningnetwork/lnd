package offers

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrMissingDescription is returned when the offer has an amount but no
	// description.
	ErrMissingDescription = errors.New("description required when amount " +
		"is set")

	// ErrMissingIssuerKey is returned when no issuer public key is
	// provided.
	ErrMissingIssuerKey = errors.New("issuer public key required")

	// ErrExpiryNotInFuture is returned when the absolute expiry is not
	// after the creation time, so no payer could use the offer.
	ErrExpiryNotInFuture = errors.New("absolute expiry is not in the " +
		"future")
)

// CreateOfferParams holds the fields of a new offer. Each field maps to a
// matching offer TLV record.
type CreateOfferParams struct {
	// Identity specifies how the offer identifies the receiver. Left holds
	// the issuer public key (offer_issuer_id), which reveals the node
	// identity. Right holds blinded message paths (offer_paths), which
	// preserve privacy.
	Identity fn.Either[*btcec.PublicKey, []lnwire.BlindedPath]

	// Description is the UTF-8 description of the payment purpose. Required
	// when Amount is set.
	Description string

	// AmountMsat is the per-item amount in millisatoshis. Zero means no
	// fixed amount (the payer must specify invreq_amount).
	AmountMsat uint64

	// AbsoluteExpiry is seconds since epoch after which the offer expires.
	// Zero leaves offer_absolute_expiry out, so the offer never expires.
	AbsoluteExpiry uint64

	// QuantityMax is the maximum items per invoice. None means the offer
	// does not support quantity selection. Some(0) means unlimited
	// quantity.
	QuantityMax fn.Option[uint64]

	// Chains specifies which blockchain networks this offer is valid for.
	// When empty, the spec defaults to Bitcoin mainnet, so non-mainnet
	// offers must set this.
	Chains [][32]byte
}

// CreateOfferResult reports a created offer. Hash is the lookup key in the
// offer store.
type CreateOfferResult struct {
	// Hash is the SHA256 hash of the TLV-encoded offer.
	Hash [32]byte

	// Encoded is the bech32-encoded offer string (lno1...).
	Encoded string
}

// CreateOffer creates and stores a BOLT 12 offer. It returns the encoded string
// and the offer hash. It rejects an absolute expiry at or before the time of
// clk.
func CreateOffer(ctx context.Context, store Store, clk clock.Clock,
	params CreateOfferParams) (*CreateOfferResult, error) {

	var identityErr error
	params.Identity.WhenLeft(func(key *btcec.PublicKey) {
		if key == nil {
			identityErr = ErrMissingIssuerKey
		}
	})
	params.Identity.WhenRight(func(paths []lnwire.BlindedPath) {
		if len(paths) == 0 {
			identityErr = fmt.Errorf("offer_paths must " +
				"contain at least one path")
		}
	})
	if identityErr != nil {
		return nil, identityErr
	}

	// The spec requires offer_description when offer_amount is set.
	if params.AmountMsat > 0 && params.Description == "" {
		return nil, ErrMissingDescription
	}

	if params.AbsoluteExpiry > 0 &&
		params.AbsoluteExpiry <= uint64(clk.Now().Unix()) {

		return nil, ErrExpiryNotInFuture
	}

	offer := &bolt12.Offer{}

	params.Identity.WhenLeft(func(key *btcec.PublicKey) {
		offer.OfferIssuerID = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]{
				Val: key,
			},
		)
	})
	params.Identity.WhenRight(func(paths []lnwire.BlindedPath) {
		offer.OfferPaths = tlv.SomeRecordT(
			tlv.RecordT[
				tlv.TlvType16, lnwire.BlindedPaths,
			]{
				Val: lnwire.BlindedPaths{
					Paths: paths,
				},
			},
		)
	})

	if len(params.Chains) > 0 {
		offer.OfferChains = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType2, bolt12.ChainsRecord]{
				Val: bolt12.ChainsRecord{
					Chains: params.Chains,
				},
			},
		)
	}

	if params.Description != "" {
		offer.OfferDescription = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType10, tlv.Blob]{
				Val: []byte(params.Description),
			},
		)
	}

	if params.AmountMsat > 0 {
		amount := bolt12.TUint64(params.AmountMsat)
		offer.OfferAmount = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType8, bolt12.TUint64]{
				Val: amount,
			},
		)
	}

	if params.AbsoluteExpiry > 0 {
		expiry := bolt12.TUint64(params.AbsoluteExpiry)
		offer.OfferAbsoluteExpiry = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType14, bolt12.TUint64]{
				Val: expiry,
			},
		)
	}

	params.QuantityMax.WhenSome(func(qty uint64) {
		offer.OfferQuantityMax = tlv.SomeRecordT(
			tlv.RecordT[tlv.TlvType20, bolt12.TUint64]{
				Val: bolt12.TUint64(qty),
			},
		)
	})

	encoded, err := bolt12.EncodeOfferString(offer)
	if err != nil {
		return nil, fmt.Errorf("encode offer: %w", err)
	}

	offerHash, err := bolt12.OfferHash(offer)
	if err != nil {
		return nil, fmt.Errorf("offer hash: %w", err)
	}

	// The encoded string carries every offer field.
	_, err = store.InsertOffer(ctx, &Offer{
		Hash:    offerHash,
		Encoded: encoded,
	})
	if err != nil {
		return nil, fmt.Errorf("persist offer: %w", err)
	}

	return &CreateOfferResult{
		Hash:    offerHash,
		Encoded: encoded,
	}, nil
}
