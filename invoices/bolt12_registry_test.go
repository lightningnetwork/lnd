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
