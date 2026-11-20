package bolt12handler

import (
	"errors"

	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/offers"
)

// ErrOfferDisabled is returned when the offer has been administratively
// disabled.
var ErrOfferDisabled = errors.New("offer is disabled")

// validateInvoiceRequestForOffer performs the offer-specific validation of an
// invoice request that ValidateInvoiceRequestRead does not cover. It checks
// that the offer is not expired or disabled, and that the offer is priced in
// msat.
//
// The caller must look up the offer by the offer hash of the request. That hit
// proves the offer fields in the request match the stored offer, so the terms
// are read from the request. The caller must also run
// bolt12.ValidateInvoiceRequestRead first, because only the reader checks the
// quantity and the amount.
func validateInvoiceRequestForOffer(ir *bolt12.InvoiceRequest,
	offer *offers.Offer, now uint64) error {

	if offer.IsDisabled {
		return ErrOfferDisabled
	}

	// Offer must not be expired. The offer expires after its last second,
	// as the payer's codec reads offer_absolute_expiry, so a request that
	// the payer could still send in that second is answered.
	if ir.OfferAbsoluteExpiry.IsSome() &&
		now > uint64(ir.OfferAbsoluteExpiry.ValOpt().UnwrapOr(0)) {

		return bolt12.ErrOfferExpired
	}

	// The reader cannot check invreq_amount against an offer_amount in
	// offer_currency units, and the handler has no exchange rate to do it.
	if ir.OfferCurrency.IsSome() {
		return errCurrencyConversion
	}

	return nil
}
