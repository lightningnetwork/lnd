// Package bolt12handler implements the BOLT 12 receiver side: handling incoming
// invoice requests, generating signed invoices, and replying via onion
// messages.
package bolt12handler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
	"golang.org/x/time/rate"
)

// InvoiceNotifier sends an Open event for each new BOLT 12 invoice to the
// connected invoice subscribers. No database row backs the event, so a
// subscriber that connects later does not get it.
type InvoiceNotifier interface {
	// NotifyNewBolt12Invoice notifies connected subscribers about a newly
	// generated BOLT 12 invoice. No database write occurs. It returns
	// without the event when ctx ends.
	NotifyNewBolt12Invoice(ctx context.Context, hash lntypes.Hash,
		invoice *invoices.Invoice)
}

// Handler processes incoming BOLT 12 invoice requests and generates signed
// invoices in response.
type Handler struct {
	offerStore         offers.Store
	notifier           InvoiceNotifier
	replier            OnionReplier
	signer             NodeSigner
	paymentPathBuilder PaymentPathBuilder
	clock              clock.Clock

	// activeChain is the genesis hash of the chain this handler serves.
	// It gates the spec invreq_chain reader rule.
	activeChain [32]byte

	// finalCLTVDelta is the final CLTV delta that this node requires for a
	// BOLT 12 invoice payment.
	finalCLTVDelta uint16

	// maxInflight is the number of invoice requests that the handler
	// answers at the same time.
	maxInflight int

	// requestLimiter limits the rate of the answered invoice requests.
	requestLimiter *rate.Limiter

	// introResolver resolves a sciddir introduction node of a reply path.
	// Start sets it before the dispatch starts.
	introResolver IntroNodeResolver

	started atomic.Bool

	// gm runs the dispatch and the invoice requests. Its Stop cancels them
	// and waits for them.
	gm *fn.GoroutineManager
}

// NewHandler creates a new BOLT 12 invoice request handler. The notifier, the
// replier and the clock must not be nil. The path builder can be nil, and the
// handler then uses a single-hop blinded path. The final CLTV delta must be at
// least routing.MinCLTVDelta, and it must leave room for routing.BlockPadding
// in a uint16.
func NewHandler(offerStore offers.Store, notifier InvoiceNotifier,
	replier OnionReplier, signer NodeSigner,
	paymentPathBuilder PaymentPathBuilder, clk clock.Clock,
	activeChain [32]byte, finalCLTVDelta uint16) (*Handler, error) {

	// The payment paths add routing.BlockPadding to the delta in a
	// uint16, so a larger delta overflows there.
	maxDelta := uint16(math.MaxUint16) - routing.BlockPadding
	if finalCLTVDelta < routing.MinCLTVDelta || finalCLTVDelta > maxDelta {
		return nil, fmt.Errorf("final CLTV delta %d is outside the "+
			"range [%d, %d]", finalCLTVDelta, routing.MinCLTVDelta,
			maxDelta)
	}

	return &Handler{
		offerStore:         offerStore,
		notifier:           notifier,
		replier:            replier,
		signer:             signer,
		paymentPathBuilder: paymentPathBuilder,
		clock:              clk,
		activeChain:        activeChain,
		finalCLTVDelta:     finalCLTVDelta,
		maxInflight:        maxInflightRequests,
		requestLimiter:     rate.NewLimiter(requestRate, requestBurst),
		gm:                 fn.NewGoroutineManager(),
	}, nil
}

// SetPaymentPathBuilder sets the payment path builder for multi-hop blinded
// payment paths. This is called after the router is initialized.
func (h *Handler) SetPaymentPathBuilder(b PaymentPathBuilder) {
	h.paymentPathBuilder = b
}

// errNoReplyPath is returned for an invoice request that arrived without a
// reply path, because the invoice then has no way back to the payer.
var errNoReplyPath = errors.New("invoice request has no reply path")

// HandleInvoiceRequest is the top-level entry point called when an onion
// message with TLV type 64 (invoice request) arrives.
func (h *Handler) HandleInvoiceRequest(ctx context.Context, invreqBytes []byte,
	replyPath *sphinx.BlindedPath) error {

	// Check the reply path first, so that a request without one costs no
	// decode and no signature.
	if replyPath == nil {
		return errNoReplyPath
	}

	ir, err := bolt12.DecodeInvoiceRequest(invreqBytes)
	if err != nil {
		return fmt.Errorf("decode invoice request: %w", err)
	}

	if err := bolt12.ValidateInvoiceRequestRead(
		ir, h.activeChain, bolt12.Bolt12Features,
	); err != nil {
		return fmt.Errorf("validate invoice request: %w", err)
	}

	offer, err := h.lookupOffer(ctx, ir)
	if err != nil {
		return fmt.Errorf("lookup offer: %w", err)
	}

	now := uint64(h.clock.Now().Unix())
	if err := validateInvoiceRequestForOffer(
		ir, offer, now,
	); err != nil {
		return fmt.Errorf("validate against offer: %w", err)
	}

	result, err := GenerateInvoice(&InvoiceConfig{
		Signer:         h.signer,
		PathBuilder:    h.paymentPathBuilder,
		Clock:          h.clock,
		FinalCLTVDelta: h.finalCLTVDelta,
	}, ir)
	if err != nil {
		return fmt.Errorf("generate invoice: %w", err)
	}

	// Send the invoice before the event and the log line, so that neither
	// announces an invoice that never left this node.
	invoiceBytes, err := result.Invoice.EncodeSigned()
	if err != nil {
		return fmt.Errorf("encode reply: %w", err)
	}

	err = h.replier.SendInvoiceReply(ctx, invoiceBytes, replyPath)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}

	log.Debugf("Issued BOLT 12 invoice for offer %d: payment_hash=%v, "+
		"amount=%v", offer.ID, result.PaymentHash,
		lnwire.MilliSatoshi(getInvoiceAmount(result.Invoice)))

	// The handler writes no invoice row and keeps the preimage nowhere, so
	// the event is the only record of the invoice. A later change adds the
	// state that a payment needs to settle the invoice.
	h.notifyInvoice(ctx, result, offer)

	return nil
}

// lookupOffer finds the stored offer that matches the invoice request's offer
// fields. It computes the offer hash from the offer fields in the request and
// looks it up in the store.
func (h *Handler) lookupOffer(ctx context.Context, ir *bolt12.InvoiceRequest) (
	*offers.Offer, error) {

	// The codec hashes the request's offer-range records, so an unknown TLV
	// the payer mirrored also moves the id. A hit therefore proves the
	// offer fields match the stored offer exactly, which is the check the
	// reader requirements ask for.
	offerHash, err := bolt12.OfferHash(ir)
	if err != nil {
		return nil, fmt.Errorf("offer hash: %w", err)
	}

	return h.offerStore.GetOfferByHash(ctx, offerHash)
}

// notifyInvoice sends the Open event of a new BOLT 12 invoice to the connected
// invoice subscribers.
func (h *Handler) notifyInvoice(ctx context.Context, result *InvoiceResult,
	offer *offers.Offer) {

	// Extract invreq_payer_id as the serialised compressed point, for the
	// invoices.Invoice struct which stores it as []byte.
	var payerIDBytes []byte
	result.Invoice.InvreqPayerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType88, *btcec.PublicKey]) {
			payerIDBytes = r.Val.SerializeCompressed()
		},
	)

	// The event carries the creation time and the features of the signed
	// invoice, so that a subscriber sees the terms the payer received.
	createdAt := result.Invoice.InvoiceCreatedAt.ValOpt().UnwrapOr(0)
	features := lnwire.EmptyFeatureVector()
	result.Invoice.InvoiceFeatures.WhenSome(
		func(r tlv.RecordT[tlv.TlvType174, lnwire.RawFeatureVector]) {
			features = lnwire.NewFeatureVector(
				&r.Val, lnwire.Features,
			)
		},
	)

	// An offer without an amount can omit offer_description, and the memo
	// is then empty.
	var memo []byte
	result.Invoice.OfferDescription.WhenSome(
		func(r tlv.RecordT[tlv.TlvType10, tlv.Blob]) {
			memo = r.Val
		},
	)

	invoice := &invoices.Invoice{
		Memo:         memo,
		CreationDate: time.Unix(int64(createdAt), 0).UTC(),
		Terms: invoices.ContractTerm{
			Expiry:          bolt12.DefaultInvoiceRelativeExpiry,
			FinalCltvDelta:  int32(h.finalCLTVDelta),
			PaymentPreimage: &result.Preimage,
			PaymentAddr:     result.PathID,
			Value: lnwire.MilliSatoshi(
				getInvoiceAmount(result.Invoice),
			),
			Features: features,
		},
		PaymentRequest: []byte(result.Encoded),
		IsBolt12:       true,
		OfferID:        fn.Some(offer.ID),
		InvreqPayerID:  payerIDBytes,
	}

	h.notifier.NotifyNewBolt12Invoice(ctx, result.PaymentHash, invoice)
}

// getInvoiceAmount returns the invoice_amount in msat, or zero if the invoice
// has none.
func getInvoiceAmount(inv *bolt12.Invoice) uint64 {
	var amt uint64
	inv.InvoiceAmount.WhenSome(
		func(r tlv.RecordT[tlv.TlvType170, bolt12.TUint64]) {
			amt = uint64(r.Val)
		},
	)

	return amt
}
