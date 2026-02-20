package lnrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidatePayReqAmt tests the ValidatePayReqAmt helper for various
// combinations of invoice amount and caller-specified amount.
func TestValidatePayReqAmt(t *testing.T) {
	t.Parallel()

	invoiceAmt := lnwire.MilliSatoshi(1_000_000)

	tests := []struct {
		name       string
		invoiceMsat *lnwire.MilliSatoshi
		amtSat     int64
		amtMsat    int64
		wantAmt    lnwire.MilliSatoshi
		wantErr    string
	}{
		{
			name:        "zero-amount invoice with amt_msat",
			invoiceMsat: nil,
			amtMsat:     500_000,
			wantAmt:     500_000,
		},
		{
			name:        "zero-amount invoice with amt_sat",
			invoiceMsat: nil,
			amtSat:      500,
			wantAmt:     500_000,
		},
		{
			name:        "zero-amount invoice with no amount",
			invoiceMsat: nil,
			wantErr:     "amount must be specified",
		},
		{
			name:        "fixed invoice, no caller amount",
			invoiceMsat: &invoiceAmt,
			wantAmt:     1_000_000,
		},
		{
			name:        "fixed invoice, exact amount",
			invoiceMsat: &invoiceAmt,
			amtMsat:     1_000_000,
			wantAmt:     1_000_000,
		},
		{
			name:        "fixed invoice, overpayment via msat",
			invoiceMsat: &invoiceAmt,
			amtMsat:     1_100_000,
			wantAmt:     1_100_000,
		},
		{
			name:        "fixed invoice, overpayment via sat",
			invoiceMsat: &invoiceAmt,
			amtSat:      1100,
			wantAmt:     1_100_000,
		},
		{
			name:        "fixed invoice, underpayment rejected",
			invoiceMsat: &invoiceAmt,
			amtMsat:     999_999,
			wantErr:     "must not be less than invoice amount",
		},
		{
			name:        "sat and msat mutually exclusive",
			invoiceMsat: &invoiceAmt,
			amtSat:      1,
			amtMsat:     1000,
			wantErr:     "mutually exclusive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ValidatePayReqAmt(
				tc.invoiceMsat, tc.amtSat, tc.amtMsat,
			)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantAmt, got)
		})
	}
}
