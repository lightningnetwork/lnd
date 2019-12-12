package invoices

import (
	"errors"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// updateResult is the result of the invoice update call.
type updateResult uint8

const (
	resultInvalid updateResult = iota
	resultReplayToCanceled
	resultReplayToAccepted
	resultReplayToSettled
	resultInvoiceAlreadyCanceled
	resultAmountTooLow
	resultExpiryTooSoon
	resultDuplicateToAccepted
	resultDuplicateToSettled
	resultAccepted
	resultSettled
	resultInvoiceNotOpen
	resultPartialAccepted
	resultMppInProgress
	resultAddressMismatch
	resultHtlcSetTotalMismatch
	resultHtlcSetTotalTooLow
	resultHtlcSetOverpayment
)

// String returns a human-readable representation of the invoice update result.
func (u updateResult) String() string {
	switch u {

	case resultInvalid:
		return "invalid"

	case resultReplayToCanceled:
		return "replayed htlc to canceled invoice"

	case resultReplayToAccepted:
		return "replayed htlc to accepted invoice"

	case resultReplayToSettled:
		return "replayed htlc to settled invoice"

	case resultInvoiceAlreadyCanceled:
		return "invoice already canceled"

	case resultAmountTooLow:
		return "amount too low"

	case resultExpiryTooSoon:
		return "expiry too soon"

	case resultDuplicateToAccepted:
		return "accepting duplicate payment to accepted invoice"

	case resultDuplicateToSettled:
		return "accepting duplicate payment to settled invoice"

	case resultAccepted:
		return "accepted"

	case resultSettled:
		return "settled"

	case resultInvoiceNotOpen:
		return "invoice no longer open"

	case resultPartialAccepted:
		return "partial payment accepted"

	case resultMppInProgress:
		return "mpp reception in progress"

	case resultAddressMismatch:
		return "payment address mismatch"

	case resultHtlcSetTotalMismatch:
		return "htlc total amt doesn't match set total"

	case resultHtlcSetTotalTooLow:
		return "set total too low for invoice"

	case resultHtlcSetOverpayment:
		return "mpp is overpaying set total"

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
	*channeldb.InvoiceUpdateDesc, updateResult, error) {

	// Don't update the invoice when this is a replayed htlc.
	htlc, ok := inv.Htlcs[ctx.circuitKey]
	if ok {
		switch htlc.State {
		case channeldb.HtlcStateCanceled:
			return nil, resultReplayToCanceled, nil

		case channeldb.HtlcStateAccepted:
			return nil, resultReplayToAccepted, nil

		case channeldb.HtlcStateSettled:
			return nil, resultReplayToSettled, nil

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
	*channeldb.InvoiceUpdateDesc, updateResult, error) {

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
		return nil, resultInvoiceNotOpen, nil
	}

	// Check the payment address that authorizes the payment.
	if ctx.mpp.PaymentAddr() != inv.Terms.PaymentAddr {
		return nil, resultAddressMismatch, nil
	}

	// Don't accept zero-valued sets.
	if ctx.mpp.TotalMsat() == 0 {
		return nil, resultHtlcSetTotalTooLow, nil
	}

	// Check that the total amt of the htlc set is high enough. In case this
	// is a zero-valued invoice, it will always be enough.
	if ctx.mpp.TotalMsat() < inv.Terms.Value {
		return nil, resultHtlcSetTotalTooLow, nil
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
			return nil, resultHtlcSetTotalMismatch, nil
		}

		newSetTotal += htlc.Amt
	}

	// Add amount of new htlc.
	newSetTotal += ctx.amtPaid

	// Make sure the communicated set total isn't overpaid.
	if newSetTotal > ctx.mpp.TotalMsat() {
		return nil, resultHtlcSetOverpayment, nil
	}

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, resultExpiryTooSoon, nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, resultExpiryTooSoon, nil
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
		return &update, resultPartialAccepted, nil
	}

	// Check to see if we can settle or this is an hold invoice and
	// we need to wait for the preimage.
	holdInvoice := inv.Terms.PaymentPreimage == channeldb.UnknownPreimage
	if holdInvoice {
		update.State = &channeldb.InvoiceStateUpdateDesc{
			NewState: channeldb.ContractAccepted,
		}
		return &update, resultAccepted, nil
	}

	update.State = &channeldb.InvoiceStateUpdateDesc{
		NewState: channeldb.ContractSettled,
		Preimage: inv.Terms.PaymentPreimage,
	}

	return &update, resultSettled, nil
}

// updateLegacy is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic for legacy payments.
func updateLegacy(ctx *invoiceUpdateCtx, inv *channeldb.Invoice) (
	*channeldb.InvoiceUpdateDesc, updateResult, error) {

	// If the invoice is already canceled, there is no further
	// checking to do.
	if inv.State == channeldb.ContractCanceled {
		return nil, resultInvoiceAlreadyCanceled, nil
	}

	// If an invoice amount is specified, check that enough is paid. Also
	// check this for duplicate payments if the invoice is already settled
	// or accepted. In case this is a zero-valued invoice, it will always be
	// enough.
	if ctx.amtPaid < inv.Terms.Value {
		return nil, resultAmountTooLow, nil
	}

	// TODO(joostjager): Check invoice mpp required feature
	// bit when feature becomes mandatory.

	// Don't allow settling the invoice with an old style
	// htlc if we are already in the process of gathering an
	// mpp set.
	for _, htlc := range inv.Htlcs {
		if htlc.State == channeldb.HtlcStateAccepted &&
			htlc.MppTotalAmt > 0 {

			return nil, resultMppInProgress, nil
		}
	}

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, resultExpiryTooSoon, nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, resultExpiryTooSoon, nil
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
		return &update, resultDuplicateToAccepted, nil

	case channeldb.ContractSettled:
		return &update, resultDuplicateToSettled, nil
	}

	// Check to see if we can settle or this is an hold invoice and we need
	// to wait for the preimage.
	holdInvoice := inv.Terms.PaymentPreimage == channeldb.UnknownPreimage
	if holdInvoice {
		update.State = &channeldb.InvoiceStateUpdateDesc{
			NewState: channeldb.ContractAccepted,
		}
		return &update, resultAccepted, nil
	}

	update.State = &channeldb.InvoiceStateUpdateDesc{
		NewState: channeldb.ContractSettled,
		Preimage: inv.Terms.PaymentPreimage,
	}

	return &update, resultSettled, nil
}
