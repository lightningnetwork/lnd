package invoices

import (
	"errors"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// ResolutionResult provides metadata which about an invoice update which can
// be used to take custom actions on resolution of the htlc. Only results which
// are actionable by the link are exported.
type ResolutionResult uint8

const (
	resultInvalid ResolutionResult = iota

	// ResultReplayToCanceled is returned when we replay a canceled invoice.
	ResultReplayToCanceled

	// ResultReplayToAccepted is returned when we replay an accepted invoice.
	ResultReplayToAccepted

	// ResultReplayToSettled is returned when we replay a settled invoice.
	ResultReplayToSettled

	// ResultInvoiceAlreadyCanceled is returned when trying to pay an invoice
	// that is already canceled.
	ResultInvoiceAlreadyCanceled

	// ResultAmountTooLow is returned when an invoice is underpaid.
	ResultAmountTooLow

	// ResultExpiryTooSoon is returned when we do not accept an invoice payment
	// because it expires too soon.
	ResultExpiryTooSoon

	// ResultDuplicateToAccepted is returned when we accept a duplicate htlc.
	ResultDuplicateToAccepted

	// ResultDuplicateToSettled is returned when we settle an invoice which has
	// already been settled at least once.
	ResultDuplicateToSettled

	// ResultAccepted is returned when we accept a hodl invoice.
	ResultAccepted

	// ResultSettled is returned when we settle an invoice.
	ResultSettled

	// ResultCanceled is returned when we cancel an invoice and its associated
	// htlcs.
	ResultCanceled

	// ResultInvoiceNotOpen is returned when a mpp invoice is not open.
	ResultInvoiceNotOpen

	// ResultPartialAccepted is returned when we have partially received
	// payment.
	ResultPartialAccepted

	// ResultMppInProgress is returned when we are busy receiving a mpp payment.
	ResultMppInProgress

	// ResultMppTimeout is returned when an invoice paid with multiple partial
	// payments times out before it is fully paid.
	ResultMppTimeout

	// ResultAddressMismatch is returned when the payment address for a mpp
	// invoice does not match.
	ResultAddressMismatch

	// ResultHtlcSetTotalMismatch is returned when the amount paid by a htlc
	// does not match its set total.
	ResultHtlcSetTotalMismatch

	// ResultHtlcSetTotalTooLow is returned when a mpp set total is too low for
	// an invoice.
	ResultHtlcSetTotalTooLow

	// ResultHtlcSetOverpayment is returned when a mpp set is overpaid.
	ResultHtlcSetOverpayment

	// ResultInvoiceNotFound is returned when an attempt is made to pay an
	// invoice that is unknown to us.
	ResultInvoiceNotFound

	// ResultKeySendError is returned when we receive invalid key send
	// parameters.
	ResultKeySendError
)

// String returns a human-readable representation of the invoice update result.
func (u ResolutionResult) String() string {
	switch u {

	case resultInvalid:
		return "invalid"

	case ResultReplayToCanceled:
		return "replayed htlc to canceled invoice"

	case ResultReplayToAccepted:
		return "replayed htlc to accepted invoice"

	case ResultReplayToSettled:
		return "replayed htlc to settled invoice"

	case ResultInvoiceAlreadyCanceled:
		return "invoice already canceled"

	case ResultAmountTooLow:
		return "amount too low"

	case ResultExpiryTooSoon:
		return "expiry too soon"

	case ResultDuplicateToAccepted:
		return "accepting duplicate payment to accepted invoice"

	case ResultDuplicateToSettled:
		return "accepting duplicate payment to settled invoice"

	case ResultAccepted:
		return "accepted"

	case ResultSettled:
		return "settled"

	case ResultInvoiceNotOpen:
		return "invoice no longer open"

	case ResultPartialAccepted:
		return "partial payment accepted"

	case ResultMppInProgress:
		return "mpp reception in progress"

	case ResultAddressMismatch:
		return "payment address mismatch"

	case ResultHtlcSetTotalMismatch:
		return "htlc total amt doesn't match set total"

	case ResultHtlcSetTotalTooLow:
		return "set total too low for invoice"

	case ResultHtlcSetOverpayment:
		return "mpp is overpaying set total"

	case ResultKeySendError:
		return "invalid key send parameters"

	default:
		return "unknown"
	}
}

// invoiceUpdateCtx is an object that describes the context for the invoice
// update to be carried out.
type invoiceUpdateCtx struct {
	circuitKey           channeldb.CircuitKey
	amtPaid              lnwire.MilliSatoshi
	expiry               uint32
	currentHeight        int32
	finalCltvRejectDelta int32
	customRecords        record.CustomSet
	mpp                  *record.MPP
}

// updateInvoice is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic.
func updateInvoice(ctx *invoiceUpdateCtx, inv *channeldb.Invoice) (
	*channeldb.InvoiceUpdateDesc, ResolutionResult, error) {

	// Don't update the invoice when this is a replayed htlc.
	htlc, ok := inv.Htlcs[ctx.circuitKey]
	if ok {
		switch htlc.State {
		case channeldb.HtlcStateCanceled:
			return nil, ResultReplayToCanceled, nil

		case channeldb.HtlcStateAccepted:
			return nil, ResultReplayToAccepted, nil

		case channeldb.HtlcStateSettled:
			return nil, ResultReplayToSettled, nil

		default:
			return nil, 0, errors.New("unknown htlc state")
		}
	}

	if ctx.mpp == nil {
		return updateLegacy(ctx, inv)
	}

	return updateMpp(ctx, inv)
}

// updateMpp is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic for mpp payments.
func updateMpp(ctx *invoiceUpdateCtx, inv *channeldb.Invoice) (
	*channeldb.InvoiceUpdateDesc, ResolutionResult, error) {

	// Start building the accept descriptor.
	acceptDesc := &channeldb.HtlcAcceptDesc{
		Amt:           ctx.amtPaid,
		Expiry:        ctx.expiry,
		AcceptHeight:  ctx.currentHeight,
		MppTotalAmt:   ctx.mpp.TotalMsat(),
		CustomRecords: ctx.customRecords,
	}

	// Only accept payments to open invoices. This behaviour differs from
	// non-mpp payments that are accepted even after the invoice is settled.
	// Because non-mpp payments don't have a payment address, this is needed
	// to thwart probing.
	if inv.State != channeldb.ContractOpen {
		return nil, ResultInvoiceNotOpen, nil
	}

	// Check the payment address that authorizes the payment.
	if ctx.mpp.PaymentAddr() != inv.Terms.PaymentAddr {
		return nil, ResultAddressMismatch, nil
	}

	// Don't accept zero-valued sets.
	if ctx.mpp.TotalMsat() == 0 {
		return nil, ResultHtlcSetTotalTooLow, nil
	}

	// Check that the total amt of the htlc set is high enough. In case this
	// is a zero-valued invoice, it will always be enough.
	if ctx.mpp.TotalMsat() < inv.Terms.Value {
		return nil, ResultHtlcSetTotalTooLow, nil
	}

	// Check whether total amt matches other htlcs in the set.
	var newSetTotal lnwire.MilliSatoshi
	for _, htlc := range inv.Htlcs {
		// Only consider accepted mpp htlcs. It is possible that there
		// are htlcs registered in the invoice database that previously
		// timed out and are in the canceled state now.
		if htlc.State != channeldb.HtlcStateAccepted {
			continue
		}

		if ctx.mpp.TotalMsat() != htlc.MppTotalAmt {
			return nil, ResultHtlcSetTotalMismatch, nil
		}

		newSetTotal += htlc.Amt
	}

	// Add amount of new htlc.
	newSetTotal += ctx.amtPaid

	// Make sure the communicated set total isn't overpaid.
	if newSetTotal > ctx.mpp.TotalMsat() {
		return nil, ResultHtlcSetOverpayment, nil
	}

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, ResultExpiryTooSoon, nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, ResultExpiryTooSoon, nil
	}

	// Record HTLC in the invoice database.
	newHtlcs := map[channeldb.CircuitKey]*channeldb.HtlcAcceptDesc{
		ctx.circuitKey: acceptDesc,
	}

	update := channeldb.InvoiceUpdateDesc{
		AddHtlcs: newHtlcs,
	}

	// If the invoice cannot be settled yet, only record the htlc.
	setComplete := newSetTotal == ctx.mpp.TotalMsat()
	if !setComplete {
		return &update, ResultPartialAccepted, nil
	}

	// Check to see if we can settle or this is an hold invoice and
	// we need to wait for the preimage.
	holdInvoice := inv.Terms.PaymentPreimage == channeldb.UnknownPreimage
	if holdInvoice {
		update.State = &channeldb.InvoiceStateUpdateDesc{
			NewState: channeldb.ContractAccepted,
		}
		return &update, ResultAccepted, nil
	}

	update.State = &channeldb.InvoiceStateUpdateDesc{
		NewState: channeldb.ContractSettled,
		Preimage: inv.Terms.PaymentPreimage,
	}

	return &update, ResultSettled, nil
}

// updateLegacy is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic for legacy payments.
func updateLegacy(ctx *invoiceUpdateCtx, inv *channeldb.Invoice) (
	*channeldb.InvoiceUpdateDesc, ResolutionResult, error) {

	// If the invoice is already canceled, there is no further
	// checking to do.
	if inv.State == channeldb.ContractCanceled {
		return nil, ResultInvoiceAlreadyCanceled, nil
	}

	// If an invoice amount is specified, check that enough is paid. Also
	// check this for duplicate payments if the invoice is already settled
	// or accepted. In case this is a zero-valued invoice, it will always be
	// enough.
	if ctx.amtPaid < inv.Terms.Value {
		return nil, ResultAmountTooLow, nil
	}

	// TODO(joostjager): Check invoice mpp required feature
	// bit when feature becomes mandatory.

	// Don't allow settling the invoice with an old style
	// htlc if we are already in the process of gathering an
	// mpp set.
	for _, htlc := range inv.Htlcs {
		if htlc.State == channeldb.HtlcStateAccepted &&
			htlc.MppTotalAmt > 0 {

			return nil, ResultMppInProgress, nil
		}
	}

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, ResultExpiryTooSoon, nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, ResultExpiryTooSoon, nil
	}

	// Record HTLC in the invoice database.
	newHtlcs := map[channeldb.CircuitKey]*channeldb.HtlcAcceptDesc{
		ctx.circuitKey: {
			Amt:           ctx.amtPaid,
			Expiry:        ctx.expiry,
			AcceptHeight:  ctx.currentHeight,
			CustomRecords: ctx.customRecords,
		},
	}

	update := channeldb.InvoiceUpdateDesc{
		AddHtlcs: newHtlcs,
	}

	// Don't update invoice state if we are accepting a duplicate payment.
	// We do accept or settle the HTLC.
	switch inv.State {
	case channeldb.ContractAccepted:
		return &update, ResultDuplicateToAccepted, nil

	case channeldb.ContractSettled:
		return &update, ResultDuplicateToSettled, nil
	}

	// Check to see if we can settle or this is an hold invoice and we need
	// to wait for the preimage.
	holdInvoice := inv.Terms.PaymentPreimage == channeldb.UnknownPreimage
	if holdInvoice {
		update.State = &channeldb.InvoiceStateUpdateDesc{
			NewState: channeldb.ContractAccepted,
		}
		return &update, ResultAccepted, nil
	}

	update.State = &channeldb.InvoiceStateUpdateDesc{
		NewState: channeldb.ContractSettled,
		Preimage: inv.Terms.PaymentPreimage,
	}

	return &update, ResultSettled, nil
}
