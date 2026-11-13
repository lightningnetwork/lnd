//go:build test_db_postgres || test_db_sqlite || test_native_sql

package invoices_test

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// bolt12Features are the features of a rebuilt BOLT 12 invoice.
var bolt12Features = lnwire.NewFeatureVector(
	lnwire.NewRawFeatureVector(
		lnwire.MPPOptional, lnwire.PaymentAddrRequired,
		lnwire.TLVOnionPayloadRequired,
	),
	lnwire.Features,
)

// errTestOfferDisabled stands in for the handler's disabled-offer error.
var errTestOfferDisabled = errors.New("offer is disabled")

// fakeReconstructor rebuilds a fixed BOLT 12 invoice, as the handler's
// reconstructor does from a valid envelope, and reports whether its offer is
// disabled.
type fakeReconstructor struct {
	invoice  *invpkg.Invoice
	disabled atomic.Bool
}

// ReconstructInvoice returns a copy of the fixed invoice, or refuses it when
// the offer is disabled.
func (f *fakeReconstructor) ReconstructInvoice(context.Context, []byte,
	chainhash.Hash, lntypes.Hash) (*invpkg.Invoice, error) {

	if f.disabled.Load() {
		return nil, errTestOfferDisabled
	}

	invoice := *f.invoice

	return &invoice, nil
}

// CheckOfferActive refuses the offer once it is disabled.
func (f *fakeReconstructor) CheckOfferActive(context.Context, []byte) error {
	if f.disabled.Load() {
		return errTestOfferDisabled
	}

	return nil
}

// newBolt12RegistryContext creates a registry over a BOLT 12 enabled SQLite
// store with one offer, and a reconstructor for an invoice of that offer. The
// options change the registry config.
func newBolt12RegistryContext(t *testing.T,
	opts ...func(*invpkg.RegistryConfig)) (*testContext, *fakeReconstructor,
	lntypes.Preimage, chainhash.Hash) {

	t.Helper()

	db := sqldb.NewTestSqliteDB(t).BaseDB
	testClock := clock.NewTestClock(testTime)

	offerHash := [32]byte{0xaa}
	offerID, err := db.Queries.InsertOffer(
		t.Context(), sqlc.InsertOfferParams{
			Hash:      offerHash[:],
			Encoded:   "lno1test",
			CreatedAt: testTime,
		},
	)
	require.NoError(t, err)

	preimage := lntypes.Preimage{0x01, 0x02}
	pathID := chainhash.Hash{0x03}
	payerID := make([]byte, 33)
	payerID[0] = 0x02

	data := testRegistryEnvelope(payerID)
	data.OfferHash = fn.Some(offerHash)
	encoded, err := envelope.Encode(data)
	require.NoError(t, err)

	reconstructor := &fakeReconstructor{
		invoice: &invpkg.Invoice{
			CreationDate: testTime,
			Terms: invpkg.ContractTerm{
				Expiry:          time.Hour,
				PaymentPreimage: &preimage,
				PaymentAddr:     pathID,
				Value:           testInvoiceAmount,
				Features:        bolt12Features,
			},
			IsBolt12:       true,
			Bolt12Envelope: encoded,
			OfferID:        fn.Some(offerID),
			OfferHash:      offerHash[:],
			InvreqPayerID:  payerID,
		},
	}

	cfg := defaultRegistryConfig()
	cfg.Bolt12Reconstructor = reconstructor
	for _, opt := range opts {
		opt(&cfg)
	}

	makeDB := func(*testing.T) (invpkg.InvoiceDB, *clock.TestClock) {
		executor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) invpkg.SQLInvoiceQueries {
				return db.WithTx(tx)
			},
		)

		store := invpkg.NewSQLStore(
			executor, testClock, invpkg.WithBolt12(),
		)

		return store, testClock
	}

	return newTestContext(t, &cfg, makeDB), reconstructor, preimage,
		pathID
}

// testRegistryEnvelope returns the envelope data of the test invoice, without
// an offer hash.
func testRegistryEnvelope(payerID []byte) *envelope.Data {
	return &envelope.Data{
		PayerID:   [33]byte(payerID),
		CreatedAt: uint64(testTime.Unix()),
		Amount:    uint64(testInvoiceAmount),
	}
}

// notifyBolt12Shard sends one MPP shard of half the invoice amount to the
// registry.
func notifyBolt12Shard(t *testing.T, ctx *testContext,
	preimage lntypes.Preimage, pathID chainhash.Hash,
	htlcID uint64) (invpkg.HtlcResolution, chan interface{}) {

	t.Helper()

	return notifyBolt12Payload(
		t, ctx, preimage, bolt12Payload(pathID, testInvoiceAmount),
		htlcID,
	)
}

// bolt12Payload returns the payload of a shard that pays through the BOLT 12
// blinded path, for an MPP set of the given total.
func bolt12Payload(pathID chainhash.Hash,
	total lnwire.MilliSatoshi) *mockPayload {

	return &mockPayload{
		mpp:          record.NewMPP(total, pathID),
		pathID:       &pathID,
		envelope:     []byte{0x01},
		totalAmtMsat: total,
	}
}

// notifyBolt12Payload sends one HTLC of half the invoice amount with the given
// payload to the registry.
func notifyBolt12Payload(t *testing.T, ctx *testContext,
	preimage lntypes.Preimage, payload *mockPayload,
	htlcID uint64) (invpkg.HtlcResolution, chan interface{}) {

	t.Helper()

	hodlChan := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		preimage.Hash(), testInvoiceAmount/2, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(htlcID), hodlChan, nil,
		payload,
	)
	require.NoError(t, err)

	return resolution, hodlChan
}

// requireNoInvoice checks that the registry has no invoice for the hash.
func requireNoInvoice(t *testing.T, ctx *testContext, hash lntypes.Hash) {
	t.Helper()

	_, err := ctx.registry.LookupInvoice(t.Context(), hash)
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)
}

// TestBolt12SettlesWhileOfferActive verifies that the reconstructed invoice
// settles when both shards of an MPP payment arrive while the offer is
// active.
func TestBolt12SettlesWhileOfferActive(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	resolution, _ = notifyBolt12Shard(t, ctx, preimage, pathID, 2)
	checkSettleResolution(t, resolution, preimage)
}

// TestBolt12RefusesShardAfterOfferDisabled verifies that a later shard is
// refused when the offer is disabled after the first shard arrived. No shard
// settles, so the payment of the disabled offer never completes.
func TestBolt12RefusesShardAfterOfferDisabled(t *testing.T) {
	t.Parallel()

	ctx, reconstructor, preimage, pathID := newBolt12RegistryContext(t)

	resolution, hodlChan := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	reconstructor.disabled.Store(true)

	resolution, _ = notifyBolt12Shard(t, ctx, preimage, pathID, 2)
	checkFailResolution(t, resolution, invpkg.ResultInvoiceNotOpen)

	// The first shard is still held, not settled.
	select {
	case <-hodlChan:
		t.Fatal("first shard was resolved")

	default:
	}

	invoice, err := ctx.registry.LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractOpen, invoice.State)
}

// TestBolt12RefusesFirstShardOfDisabledOffer verifies that no invoice is
// created when the first shard arrives for a disabled offer.
func TestBolt12RefusesFirstShardOfDisabledOffer(t *testing.T) {
	t.Parallel()

	ctx, reconstructor, preimage, pathID := newBolt12RegistryContext(t)
	reconstructor.disabled.Store(true)

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	checkFailResolution(t, resolution, invpkg.ResultInvoiceNotFound)

	_, err := ctx.registry.LookupInvoice(t.Context(), preimage.Hash())
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)
}

// TestBolt12SettlesInvoiceWithoutOffer verifies that the disabled-offer check
// skips an invoice that answers an invoice request without an offer. Such an
// invoice is stored when it is created, so it never goes through the
// reconstructor, and it has no offer to disable.
func TestBolt12SettlesInvoiceWithoutOffer(t *testing.T) {
	t.Parallel()

	ctx, reconstructor, preimage, pathID := newBolt12RegistryContext(t)

	// The reconstructor reports every offer as disabled, so a settled
	// payment proves that the check did not run.
	reconstructor.disabled.Store(true)

	encoded, err := envelope.Encode(
		testRegistryEnvelope(reconstructor.invoice.InvreqPayerID),
	)
	require.NoError(t, err)

	invoice := *reconstructor.invoice
	invoice.OfferID = fn.None[int64]()
	invoice.OfferHash = nil
	invoice.Bolt12Envelope = encoded
	_, err = ctx.registry.AddInvoice(t.Context(), &invoice, preimage.Hash())
	require.NoError(t, err)

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	resolution, _ = notifyBolt12Shard(t, ctx, preimage, pathID, 2)
	checkSettleResolution(t, resolution, preimage)
}

// TestBolt12RebuildNeedsEnvelopeAndPathID verifies that an HTLC for an unknown
// hash rebuilds no invoice unless it carries both the invoice envelope and the
// payment path id. A BOLT 11 blinded path has a path id and no envelope.
func TestBolt12RebuildNeedsEnvelopeAndPathID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		noEnvelope bool
		noPathID   bool
	}{
		{
			name:       "no envelope",
			noEnvelope: true,
		},
		{
			name:     "no path id",
			noPathID: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, _, preimage, pathID := newBolt12RegistryContext(t)

			payload := bolt12Payload(pathID, testInvoiceAmount)
			if test.noEnvelope {
				payload.envelope = nil
			}
			if test.noPathID {
				payload.pathID = nil
			}

			resolution, _ := notifyBolt12Payload(
				t, ctx, preimage, payload, 1,
			)
			checkFailResolution(
				t, resolution, invpkg.ResultInvoiceNotFound,
			)

			requireNoInvoice(t, ctx, preimage.Hash())
		})
	}
}

// TestBolt12SendsOneOpenEvent verifies that an all-invoice subscriber gets one
// Open event for a rebuilt BOLT 12 invoice, with the add index of the stored
// row and no HTLC, and then the Settled event. A BOLT 11 invoice that is added
// after it gets the next add index.
func TestBolt12SendsOneOpenEvent(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	subscription, err := ctx.registry.SubscribeNotifications(
		t.Context(), 0, 0,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	receive := func(events chan *invpkg.Invoice) *invpkg.Invoice {
		t.Helper()

		select {
		case invoice := <-events:
			return invoice

		case <-time.After(testTimeout):
			t.Fatal("no invoice event")

			return nil
		}
	}

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	stored, err := ctx.registry.LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)

	opened := receive(subscription.NewInvoices)
	require.True(t, opened.IsBolt12)
	require.Equal(t, invpkg.ContractOpen, opened.State)
	require.Equal(t, stored.AddIndex, opened.AddIndex)
	require.Empty(t, opened.Htlcs)

	resolution, _ = notifyBolt12Shard(t, ctx, preimage, pathID, 2)
	checkSettleResolution(t, resolution, preimage)

	// The settle moves the invoice to PendingSettle, which sends no
	// event. The Settled event comes when both shards have their final
	// outcome.
	ref := invpkg.InvoiceRefByHash(preimage.Hash())
	ctx.finalizeHtlc(ref, getCircuitKey(1), true)
	ctx.finalizeHtlc(ref, getCircuitKey(2), true)

	settled := receive(subscription.SettledInvoices)
	require.True(t, settled.IsBolt12)
	require.Equal(t, stored.AddIndex, settled.AddIndex)

	_, err = ctx.registry.AddInvoice(
		t.Context(), newInvoice(t, false, false),
		testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// The rebuilt invoice sent one Open event, so the next one is the
	// BOLT 11 invoice, with the next add index.
	bolt11 := receive(subscription.NewInvoices)
	require.False(t, bolt11.IsBolt12)
	require.Equal(t, stored.AddIndex+1, bolt11.AddIndex)
}

// TestBolt12SingleSubscriberOneOpenEvent verifies that only the first HTLC of
// a rebuilt BOLT 12 invoice sends an Open event. A subscriber to the single
// invoice gets that event and then the update of the settling HTLC.
func TestBolt12SingleSubscriberOneOpenEvent(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	subscription, err := ctx.registry.SubscribeSingleInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	resolution, _ = notifyBolt12Shard(t, ctx, preimage, pathID, 2)
	require.NotNil(t, resolution)

	receive := func() *invpkg.Invoice {
		t.Helper()

		select {
		case invoice := <-subscription.Updates:
			return invoice

		case <-time.After(testTimeout):
			t.Fatal("no invoice update")

			return nil
		}
	}

	require.Equal(t, invpkg.ContractOpen, receive().State)
	require.Equal(t, invpkg.ContractPendingSettle, receive().State)
}

// TestBolt12DeletesRowAfterMppTimeout verifies that the registry deletes the
// rebuilt invoice when the MPP timeout cancels its only accepted HTLC.
func TestBolt12DeletesRowAfterMppTimeout(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	resolution, hodlChan := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	ctx.clock.SetTime(testTime.Add(30 * time.Second))

	select {
	case res := <-hodlChan:
		checkFailResolution(
			t, res.(invpkg.HtlcResolution),
			invpkg.ResultMppTimeout,
		)

	case <-time.After(testTimeout):
		t.Fatal("no MPP timeout")
	}

	// The event loop deletes the row after it resolves the HTLC.
	require.Eventually(t, func() bool {
		_, err := ctx.registry.LookupInvoice(
			t.Context(), preimage.Hash(),
		)

		return errors.Is(err, invpkg.ErrInvoiceNotFound)
	}, testTimeout, 10*time.Millisecond)
}

// TestBolt12DeletesRowWhenFirstShardFails verifies that the registry deletes
// the rebuilt invoice on each outcome that does not accept the first shard. No
// HTLC pays the row and the expiry watcher never gets it, so it would stay open
// forever.
func TestBolt12DeletesRowWhenFirstShardFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// interceptErr and cancelSet set the response of the HTLC
		// interceptor.
		interceptErr error
		cancelSet    bool

		total lnwire.MilliSatoshi
		want  invpkg.FailResolutionResult
	}{
		{
			// The update refuses the shard after the registry
			// stored the rebuilt invoice.
			name:  "total below the amount",
			total: testInvoiceAmount / 2,
			want:  invpkg.ResultHtlcSetTotalTooLow,
		},
		{
			name:      "interceptor cancels the set",
			cancelSet: true,
			total:     testInvoiceAmount,
			want:      invpkg.ExternalValidationFailed,
		},
		{
			name:         "interceptor fails",
			interceptErr: errors.New("interceptor down"),
			total:        testInvoiceAmount,
			want:         invpkg.ResultInvoiceInterceptorError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var resp any
			if test.interceptErr == nil {
				resp = invpkg.HtlcModifyResponse{
					CancelSet: test.cancelSet,
				}
			}

			interceptor := &invpkg.MockHtlcModifier{}
			interceptor.On(
				"Intercept", mock.Anything, mock.Anything,
			).Return(test.interceptErr, resp)

			ctx, _, preimage, pathID := newBolt12RegistryContext(
				t, func(cfg *invpkg.RegistryConfig) {
					cfg.HtlcInterceptor = interceptor
				},
			)

			payload := bolt12Payload(pathID, test.total)
			resolution, _ := notifyBolt12Payload(
				t, ctx, preimage, payload, 1,
			)
			checkFailResolution(t, resolution, test.want)

			requireNoInvoice(t, ctx, preimage.Hash())
		})
	}
}

// TestBolt12NoOpenEventForDeletedRow verifies that a rebuilt invoice that the
// registry deletes again sends no Open event, so a subscriber never sees an
// invoice that does not exist.
func TestBolt12NoOpenEventForDeletedRow(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	subscription, err := ctx.registry.SubscribeNotifications(
		t.Context(), 0, 0,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	// The MPP total is below the invoice amount, so the registry deletes
	// the rebuilt row.
	payload := bolt12Payload(pathID, testInvoiceAmount/2)
	resolution, _ := notifyBolt12Payload(t, ctx, preimage, payload, 1)
	checkFailResolution(t, resolution, invpkg.ResultHtlcSetTotalTooLow)

	// The next event is the one of a BOLT 11 invoice added after it.
	_, err = ctx.registry.AddInvoice(
		t.Context(), newInvoice(t, false, false),
		testInvoicePaymentHash,
	)
	require.NoError(t, err)

	select {
	case invoice := <-subscription.NewInvoices:
		require.False(t, invoice.IsBolt12)

	case <-time.After(testTimeout):
		t.Fatal("no invoice event")
	}
}

// TestBolt12LaterShardExpiry verifies that a later shard settles up to the last
// second of the stored invoice and is refused after it. The rebuild checks the
// expiry only for the first shard.
func TestBolt12LaterShardExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		elapsed time.Duration
		settles bool
	}{
		{
			name:    "last second",
			elapsed: 10 * time.Second,
			settles: true,
		},
		{
			name:    "after expiry",
			elapsed: 11 * time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, reconstructor, preimage, pathID :=
				newBolt12RegistryContext(t)

			// The invoice expires before the MPP timeout, so the
			// first shard is still held when the second arrives.
			reconstructor.invoice.Terms.Expiry = 10 * time.Second

			resolution, _ := notifyBolt12Shard(
				t, ctx, preimage, pathID, 1,
			)
			require.Nil(t, resolution)

			ctx.clock.SetTime(testTime.Add(test.elapsed))
			resolution, _ = notifyBolt12Shard(
				t, ctx, preimage, pathID, 2,
			)
			if test.settles {
				checkSettleResolution(t, resolution, preimage)

				return
			}
			checkFailResolution(
				t, resolution, invpkg.ResultInvoiceNotOpen,
			)
		})
	}
}

// TestBolt12RefusesHtlcWithoutPathID verifies that a BOLT 12 invoice refuses a
// plain HTLC that does not pay through its blinded path, even when the HTLC
// pays the whole amount.
func TestBolt12RefusesHtlcWithoutPathID(t *testing.T) {
	t.Parallel()

	ctx, _, preimage, pathID := newBolt12RegistryContext(t)

	resolution, _ := notifyBolt12Shard(t, ctx, preimage, pathID, 1)
	require.Nil(t, resolution)

	resolution, err := ctx.registry.NotifyExitHopHtlc(
		preimage.Hash(), testInvoiceAmount, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(2), nil, nil, &mockPayload{},
	)
	require.NoError(t, err)
	checkFailResolution(t, resolution, invpkg.ResultAddressMismatch)

	invoice, err := ctx.registry.LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractOpen, invoice.State)
}
