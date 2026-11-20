package invoices_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/graph/db/models"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// testSettleCollection checks persisted collection outcomes through each
// invoice database backend, including failures followed by later successes.
func testSettleCollection(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()

	const value = lnwire.MilliSatoshi(1_000_000)
	tests := []struct {
		name        string
		outcomes    [2]bool
		batch       bool
		preCanceled bool
		perHTLC     lnwire.MilliSatoshi
		wantState   invpkg.ContractState
	}{
		{
			name:      "failure first",
			outcomes:  [2]bool{false, true},
			perHTLC:   value / 2,
			wantState: invpkg.ContractCanceled,
		},
		{
			name:      "all success",
			outcomes:  [2]bool{true, true},
			perHTLC:   value / 2,
			wantState: invpkg.ContractSettled,
		},
		{
			name:      "all failed",
			outcomes:  [2]bool{false, false},
			perHTLC:   value / 2,
			wantState: invpkg.ContractCanceled,
		},
		{
			name:      "mixed batch",
			outcomes:  [2]bool{true, false},
			batch:     true,
			perHTLC:   value / 2,
			wantState: invpkg.ContractCanceled,
		},
		{
			name:      "overpayment with loss",
			outcomes:  [2]bool{true, false},
			batch:     true,
			perHTLC:   3 * value / 2,
			wantState: invpkg.ContractCanceled,
		},
		{
			name:        "prior canceled attempt",
			outcomes:    [2]bool{true, true},
			preCanceled: true,
			perHTLC:     value / 2,
			wantState:   invpkg.ContractSettled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange: Persist a two-shard payment awaiting
			// final outcomes. Optionally precede it with a
			// canceled attempt to check later collections.
			ctx := t.Context()
			db := makeDB(t)
			invoice, err := randInvoice(value)
			require.NoError(t, err)
			payHash := invoice.Terms.PaymentPreimage.Hash()
			_, err = db.AddInvoice(ctx, invoice, payHash)
			require.NoError(t, err)
			ref := invpkg.InvoiceRefByHash(payHash)
			apply := func(update *invpkg.InvoiceUpdateDesc) {
				callback := func(*invpkg.Invoice) (
					*invpkg.InvoiceUpdateDesc, error) {

					return update, nil
				}
				_, err := db.UpdateInvoice(
					ctx, ref, nil, callback,
				)
				require.NoError(t, err)
			}

			canceledKey := models.CircuitKey{HtlcID: 0}
			keys := [2]models.CircuitKey{{HtlcID: 1}, {HtlcID: 2}}
			accepts := make(
				map[models.CircuitKey]*invpkg.HtlcAcceptDesc,
			)
			for _, key := range append(keys[:], canceledKey) {
				accepts[key] = &invpkg.HtlcAcceptDesc{
					Amt:           test.perHTLC,
					CustomRecords: make(record.CustomSet),
				}
			}

			// A timed-out attempt must not poison a later payment.
			prior := make(
				map[models.CircuitKey]*invpkg.HtlcAcceptDesc,
			)
			if test.preCanceled {
				prior[canceledKey] = accepts[canceledKey]
				apply(&invpkg.InvoiceUpdateDesc{
					UpdateType: invpkg.AddHTLCsUpdate,
					AddHtlcs:   prior,
				})
				cancels := map[models.CircuitKey]struct{}{
					canceledKey: {},
				}
				apply(&invpkg.InvoiceUpdateDesc{
					UpdateType:  invpkg.CancelHTLCsUpdate,
					CancelHtlcs: cancels,
				})
			}
			delete(accepts, canceledKey)
			state := &invpkg.InvoiceStateUpdateDesc{
				NewState: invpkg.ContractPendingSettle,
				Preimage: invoice.Terms.PaymentPreimage,
			}
			apply(&invpkg.InvoiceUpdateDesc{
				UpdateType: invpkg.AddHTLCsUpdate,
				AddHtlcs:   accepts,
				State:      state,
			})

			// Act: Apply single outcomes in order; batch cases
			// defer the update until both outcomes are ready.
			var collected lnwire.MilliSatoshi
			for n, success := range test.outcomes {
				if success {
					collected += test.perHTLC
				}
				if test.batch {
					continue
				}

				_, err = db.UpdateInvoice(ctx, ref, nil,
					getFinalizeHtlcs(success, keys[n]),
				)
				require.NoError(t, err)

				// Assert: Reload before the remaining shard
				// finishes; the invoice must stay pending.
				stored, err := db.LookupInvoice(ctx, ref)
				require.NoError(t, err)
				htlcState := stored.Htlcs[keys[n]].State
				if n == 0 {
					require.Equal(
						t, invpkg.ContractPendingSettle,
						stored.State,
					)
					require.Equal(t, collected+test.perHTLC,
						stored.AmtPaid)
					require.Zero(t, stored.SettleIndex)
					require.True(
						t, stored.SettleDate.IsZero(),
					)
					want := invpkg.HtlcStateFailed
					if success {
						want = invpkg.HtlcStateSettled
					}
					require.Equal(t, want, htlcState)
				}
			}

			if test.batch {
				// Act: Submit both outcomes together to check
				// collection within a single database update.
				finalize := map[models.CircuitKey]bool{
					keys[0]: test.outcomes[0],
					keys[1]: test.outcomes[1],
				}
				update := &invpkg.InvoiceUpdateDesc{
					UpdateType: invpkg.
						FinalizeHTLCsUpdate,
					FinalizeHtlcs: finalize,
				}
				callback := func(*invpkg.Invoice) (
					*invpkg.InvoiceUpdateDesc, error) {

					return update, nil
				}
				_, err = db.UpdateInvoice(
					ctx, ref, nil, callback,
				)
				require.NoError(t, err)
			}

			// Assert: Reload to check final state, receipts,
			// settlement metadata and stored HTLC outcomes.
			stored, err := db.LookupInvoice(ctx, ref)
			require.NoError(t, err)
			require.Equal(t, test.wantState, stored.State)
			require.Equal(t, collected, stored.AmtPaid)
			if test.wantState == invpkg.ContractSettled {
				require.NotZero(t, stored.SettleIndex)
				require.False(t, stored.SettleDate.IsZero())
			} else {
				require.Zero(t, stored.SettleIndex)
				require.True(t, stored.SettleDate.IsZero())
			}
			for n, key := range keys {
				want := invpkg.HtlcStateFailed
				if test.outcomes[n] {
					want = invpkg.HtlcStateSettled
				}
				require.Equal(t, want, stored.Htlcs[key].State)
			}
			if test.preCanceled {
				require.Equal(t, invpkg.HtlcStateCanceled,
					stored.Htlcs[canceledKey].State)
			}
		})
	}
}
