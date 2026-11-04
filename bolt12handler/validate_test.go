package bolt12handler

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestValidateInvoiceRequestForOffer verifies the offer-specific checks. A
// request that arrives in the offer_absolute_expiry second passes, as the
// payer's codec still accepts the offer then. A request for an offer priced in
// a currency fails, because its invreq_amount in msat cannot be compared with
// offer_amount.
func TestValidateInvoiceRequestForOffer(t *testing.T) {
	t.Parallel()

	const expiry = 1735689600

	var payerSeed [32]byte
	payerSeed[0] = 0xFF
	payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

	tests := []struct {
		name     string
		expiry   uint64
		currency bool
		disabled bool
		now      uint64
		wantErr  error
	}{
		{name: "valid", now: expiry},
		{
			name:     "disabled",
			disabled: true,
			now:      expiry,
			wantErr:  ErrOfferDisabled,
		},
		{name: "expiry second", expiry: expiry, now: expiry},
		{
			name:    "expired",
			expiry:  expiry,
			now:     expiry + 1,
			wantErr: bolt12.ErrOfferExpired,
		},
		{
			name:     "currency",
			currency: true,
			now:      expiry,
			wantErr:  errCurrencyConversion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			offer := testOffer(t, testKey(t), 1000)
			if tc.expiry > 0 {
				expiry := bolt12.TUint64(tc.expiry)
				offer.OfferAbsoluteExpiry = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType14](
						expiry,
					),
				)
			}

			if tc.currency {
				offer.OfferCurrency = tlv.SomeRecordT(
					tlv.RecordT[tlv.TlvType6, tlv.Blob]{
						Val: []byte("USD"),
					},
				)
			}

			ir := testInvoiceRequest(t, offer, payerKey, 1000)

			err := validateInvoiceRequestForOffer(
				ir, &offers.Offer{IsDisabled: tc.disabled},
				tc.now,
			)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}
