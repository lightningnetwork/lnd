package invoices

import (
	"errors"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// UpdateResult is the result of the invoice update call.
type UpdateResult uint8

const (
	ResultInvalid UpdateResult = iota
	ResultReplayToCanceled
	ResultReplayToAccepted
	ResultReplayToSettled
	ResultInvoiceAlreadyCanceled
	ResultAmountTooLow
	ResultExpiryTooSoon
	ResultDuplicateToAccepted
	ResultDuplicateToSettled
	ResultAccepted
	ResultSettled
)

// String returns a human-readable representation of the invoice update result.
func (u UpdateResult) String() string {
	switch u {

	case ResultInvalid:
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
}

// updateInvoice is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic.
func updateInvoice(ctx *invoiceUpdateCtx, inv *channeldb.Invoice) (
	*channeldb.InvoiceUpdateDesc, UpdateResult, error) {

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

	// If the invoice is already canceled, there is no further checking to
	// do.
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
			Amt:          ctx.amtPaid,
			Expiry:       ctx.expiry,
			AcceptHeight: ctx.currentHeight,
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
