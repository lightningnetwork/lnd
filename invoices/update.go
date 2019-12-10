package invoices

import (
	"errors"

	"github.com/lightningnetwork/lnd/htlcswitch/hop"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
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
	customRecords        hop.CustomRecordSet
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

	// If the invoice is already canceled, there is no further checking to
	// do.
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
