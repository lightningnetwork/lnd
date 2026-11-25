package bolt12handler

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// errForeignPathID is returned for a payment whose path id this node
	// did not make for its invoice envelope.
	errForeignPathID = errors.New("payment path id does not match the " +
		"invoice envelope")

	// errNoOfferInEnvelope is returned for a payment whose invoice
	// envelope names no offer.
	errNoOfferInEnvelope = errors.New("invoice envelope names no offer")

	// errAmountBelowOffer is returned for an invoice envelope whose amount
	// is below the price of the offer.
	errAmountBelowOffer = errors.New("invoice amount below the offer " +
		"price")
)

// Reconstructor implements invoices.Bolt12Reconstructor. It builds an
// invoices.Invoice from an invoice envelope and the offer store. It refuses
// an envelope that the payment path id does not authenticate.
type Reconstructor struct {
	// pathIDKey checks the payment path ids that this node made.
	pathIDKey  PathIDKey
	offerStore offers.Store

	// activeChain is the genesis hash of the chain of this node. The
	// offer reader checks the stored offer against it.
	activeChain [32]byte

	// finalCLTVDelta is the final CLTV delta of the invoices, see
	// InvoiceConfig. The invoice does not carry it, so a changed config
	// value also applies to the invoices that are already out.
	finalCLTVDelta uint16
}

// NewReconstructor creates a Reconstructor that checks payment path ids with
// pathIDKey and reads offers from the offer store.
func NewReconstructor(pathIDKey PathIDKey, offerStore offers.Store,
	activeChain [32]byte, finalCLTVDelta uint16) *Reconstructor {

	return &Reconstructor{
		pathIDKey:      pathIDKey,
		offerStore:     offerStore,
		activeChain:    activeChain,
		finalCLTVDelta: finalCLTVDelta,
	}
}

// ReconstructInvoice rebuilds a fully populated Invoice from the invoice
// envelope of a payment, after it checks the payment path id.
//
// NOTE: This is part of the invoices.Bolt12Reconstructor interface.
func (r *Reconstructor) ReconstructInvoice(ctx context.Context,
	envelopeBytes []byte, pathID chainhash.Hash,
	paymentHash lntypes.Hash) (*invoices.Invoice, error) {

	// Only this node can make a path id that matches the envelope, and
	// the same check gives the preimage. So a payer can neither build nor
	// change an envelope.
	preimage, ok := r.pathIDKey.checkPaymentPathID(
		pathID[:], EnvelopeBinding(envelopeBytes),
	)
	if !ok {
		return nil, errForeignPathID
	}

	if preimage.Hash() != paymentHash {
		return nil, fmt.Errorf("preimage hash mismatch: computed %v, "+
			"expected %v", preimage.Hash(), paymentHash)
	}

	data, err := envelope.Decode(envelopeBytes)
	if err != nil {
		return nil, fmt.Errorf("decode invoice envelope: %w", err)
	}

	// An invoice that answers no offer is stored when it is made, so only
	// an envelope that names an offer can be rebuilt here.
	offerHash, err := data.OfferHash.UnwrapOrErr(errNoOfferInEnvelope)
	if err != nil {
		return nil, err
	}

	// The invoice expires after its last second. The payer's codec reads
	// invoice_relative_expiry, so a payment in that second settles.
	relExpiry := uint64(bolt12.DefaultInvoiceRelativeExpiry / time.Second)
	expiryTime := data.CreatedAt + relExpiry
	now := uint64(time.Now().Unix())
	if now > expiryTime {
		return nil, fmt.Errorf("envelope expired: created_at=%d, "+
			"expiry=%d, now=%d",
			data.CreatedAt, expiryTime, now)
	}

	offer, err := r.offerStore.GetOfferByHash(ctx, offerHash)
	if err != nil {
		return nil, fmt.Errorf("lookup offer: %w", err)
	}

	// A disabled offer takes no new payments. This check runs only for the
	// first HTLC of an invoice. No HTLC settles before the whole set is
	// accepted, so refusing here settles nothing for the offer.
	if offer.IsDisabled {
		return nil, ErrOfferDisabled
	}

	// The offer is decoded as of the creation time of the invoice. The
	// handler checked the offer expiry at that time. An offer that expired
	// since then must not fail the payment.
	decoded, err := bolt12.DecodeOfferString(
		offer.Encoded, time.Unix(int64(data.CreatedAt), 0),
		r.activeChain,
	)
	if err != nil {
		return nil, fmt.Errorf("decode offer %x: %w", offer.Hash[:],
			err)
	}

	if err := checkOfferPrice(decoded, data); err != nil {
		return nil, err
	}

	invoice := &invoices.Invoice{
		Memo:         offerMemo(decoded),
		CreationDate: time.Unix(int64(data.CreatedAt), 0).UTC(),
		Terms: invoices.ContractTerm{
			Expiry:          bolt12.DefaultInvoiceRelativeExpiry,
			FinalCltvDelta:  int32(r.finalCLTVDelta),
			PaymentPreimage: &preimage,
			PaymentAddr:     pathID,
			Value:           lnwire.MilliSatoshi(data.Amount),

			// An issued invoice advertises basic MPP, so the
			// rebuilt invoice has it and the features that MPP
			// depends on. The payment path id is the payment
			// address, so the address is required.
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.MPPOptional,
					lnwire.PaymentAddrRequired,
					lnwire.TLVOnionPayloadRequired,
				),
				lnwire.Features,
			),
		},
		IsBolt12:        true,
		Bolt12Envelope:  envelopeBytes,
		OfferID:         fn.Some(offer.ID),
		OfferHash:       offer.Hash[:],
		InvreqPayerID:   data.PayerID[:],
		InvreqQuantity:  data.Quantity,
		InvreqPayerNote: data.PayerNote,
	}

	return invoice, nil
}

// checkOfferPrice returns an error when the envelope amount is below the
// price of the offer. The price is offer_amount times the quantity, and no
// quantity counts as one item.
func checkOfferPrice(offer *bolt12.Offer, data *envelope.Data) error {
	// The handler answers no request for an offer priced in a currency.
	// Only a forged envelope names one, and it fails with
	// errCurrencyConversion.
	if offer.OfferCurrency.IsSome() {
		return errCurrencyConversion
	}

	// An offer without a price lets the payer choose the amount, so it is
	// not checked.
	if offer.OfferAmount.IsNone() {
		return nil
	}

	offerAmount := offer.OfferAmount.ValOpt().UnwrapOr(0)

	quantity := data.Quantity
	if quantity == 0 {
		quantity = 1
	}

	price, err := expectedOfferAmount(uint64(offerAmount), quantity)
	if err != nil {
		return err
	}

	// A valid envelope carries an amount from a checked request, so only a
	// forged envelope fails here.
	if data.Amount < price {
		return fmt.Errorf("%w: amount %d, price %d",
			errAmountBelowOffer, data.Amount, price)
	}

	return nil
}

// offerMemo returns the offer_description of the offer, which is the memo of
// the rebuilt invoice.
func offerMemo(offer *bolt12.Offer) []byte {
	var memo []byte
	offer.OfferDescription.WhenSome(
		func(rec tlv.RecordT[tlv.TlvType10, tlv.Blob]) {
			memo = rec.Val
		},
	)

	return truncateMemo(memo)
}

// truncateMemo cuts the memo to invoices.MaxMemoSize bytes, because the
// invoice store refuses a longer memo. An onion message can carry a longer
// offer_description. The cut does not split a UTF-8 rune.
func truncateMemo(memo []byte) []byte {
	if len(memo) <= invoices.MaxMemoSize {
		return memo
	}

	cut := invoices.MaxMemoSize
	for cut > 0 && !utf8.RuneStart(memo[cut]) {
		cut--
	}

	return memo[:cut]
}

// CheckOfferActive returns an error when the offer with the given hash is
// unknown or disabled.
//
// NOTE: This is part of the invoices.Bolt12Reconstructor interface.
func (r *Reconstructor) CheckOfferActive(ctx context.Context,
	offerHash []byte) error {

	if len(offerHash) != 32 {
		return fmt.Errorf("invalid offer hash length %d",
			len(offerHash))
	}

	offer, err := r.offerStore.GetOfferByHash(ctx, [32]byte(offerHash))
	if err != nil {
		return fmt.Errorf("lookup offer: %w", err)
	}

	if offer.IsDisabled {
		return ErrOfferDisabled
	}

	return nil
}

// Compile-time check that Reconstructor implements
// invoices.Bolt12Reconstructor.
var _ invoices.Bolt12Reconstructor = (*Reconstructor)(nil)
