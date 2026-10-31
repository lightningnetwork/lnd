package bolt12handler

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestHandleInvoiceRequest_FullFlow verifies that the handler replies with a
// signed invoice and notifies an Open event that carries the terms of that
// invoice.
func TestHandleInvoiceRequest_FullFlow(t *testing.T) {
	t.Parallel()

	nodeKey := testKey(t)
	store := newMockOfferStore()
	notifier := &mockNotifier{}
	replier := &mockReplier{}

	// The builder records the path id and fails, so the handler uses the
	// single-hop path.
	builder := &fakePathBuilder{err: errors.New("no paths")}

	handler, err := NewHandler(
		store, notifier, replier, newPrivKeySigner(nodeKey),
		builder, clock.NewDefaultClock(), testChainHash(),
		testFinalCLTVDelta,
	)
	require.NoError(t, err)

	// Create and store an offer.
	offer, stored := addOffer(t, store, nodeKey, 10000)

	// Build a signed invoice request.
	var payerSeed [32]byte
	payerSeed[0] = 0xFF
	payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

	invreqBytes := buildSignedInvreqBytes(
		t, offer, payerKey, 10000,
	)

	// Create a dummy reply path.
	replyPath := &sphinx.BlindedPath{
		IntroductionPoint: nodeKey.PubKey(),
	}

	// Handle the request.
	ctx := t.Context()
	err = handler.HandleInvoiceRequest(ctx, invreqBytes, replyPath)
	require.NoError(t, err)

	// Verify invoice notification was sent (no DB write).
	require.Len(t, notifier.invoices, 1)
	inv := notifier.invoices[0]
	require.True(t, inv.IsBolt12)
	require.Equal(t, fn.Some(stored.ID), inv.OfferID)
	require.Equal(t, []byte("test offer"), inv.Memo)
	require.Equal(t,
		lnwire.MilliSatoshi(10000), inv.Terms.Value,
	)

	// Verify the preimage is set and hashes to the registered payment hash.
	require.NotNil(t, inv.Terms.PaymentPreimage)
	expectedHash := inv.Terms.PaymentPreimage.Hash()
	require.Equal(t, expectedHash, notifier.hashes[0])

	// The reply carries the signed invoice, and the event must match its
	// terms.
	require.Len(t, replier.replies, 1)
	sent, err := bolt12.DecodeInvoice(replier.replies[0])
	require.NoError(t, err)

	createdAt := sent.InvoiceCreatedAt.UnwrapOrFailV(t)
	require.Equal(t, int64(createdAt), inv.CreationDate.Unix())

	paymentHash := sent.InvoicePaymentHash.UnwrapOrFailV(t)
	require.Equal(t, lntypes.Hash(paymentHash), notifier.hashes[0])

	amount := sent.InvoiceAmount.UnwrapOrFailV(t)
	require.EqualValues(t, amount, inv.Terms.Value)

	features := sent.InvoiceFeatures.UnwrapOrFailV(t)
	require.Equal(t,
		lnwire.NewFeatureVector(&features, lnwire.Features),
		inv.Terms.Features,
	)
	require.True(t, inv.Terms.Features.HasFeature(lnwire.MPPOptional))

	encoded, err := bolt12.EncodeInvoiceString(sent)
	require.NoError(t, err)
	require.Equal(t, encoded, string(inv.PaymentRequest))

	require.True(t, sent.InvoiceRelativeExp.IsNone())
	require.Equal(t, bolt12.DefaultInvoiceRelativeExpiry, inv.Terms.Expiry)
	require.EqualValues(t, testFinalCLTVDelta, inv.Terms.FinalCltvDelta)

	// The single-hop path asks the payer for the delta plus the padding.
	payInfos := sent.InvoiceBlindedPay.UnwrapOrFailV(t).Infos
	require.Len(t, payInfos, 1)
	require.Equal(t,
		testFinalCLTVDelta+routing.BlockPadding,
		payInfos[0].CltvExpiryDelta,
	)
	require.Equal(t, builder.gotPathID, inv.Terms.PaymentAddr[:])
	require.Equal(t,
		payerKey.PubKey().SerializeCompressed(), inv.InvreqPayerID,
	)
}

// mockOfferStore implements offers.Store for testing.
type mockOfferStore struct {
	offers map[[32]byte]*offers.Offer
}

func newMockOfferStore() *mockOfferStore {
	return &mockOfferStore{
		offers: make(map[[32]byte]*offers.Offer),
	}
}

func (m *mockOfferStore) InsertOffer(_ context.Context, offer *offers.Offer) (
	int64, error) {

	m.offers[offer.Hash] = offer
	offer.ID = int64(len(m.offers))

	return offer.ID, nil
}

func (m *mockOfferStore) GetOfferByHash(_ context.Context,
	offerHash [32]byte) (*offers.Offer, error) {

	if o, ok := m.offers[offerHash]; ok {
		return o, nil
	}

	return nil, offers.ErrOfferNotFound
}

// mockNotifier captures BOLT 12 invoice notifications during the handler flow.
type mockNotifier struct {
	invoices []*invoices.Invoice
	hashes   []lntypes.Hash
}

func (m *mockNotifier) NotifyNewBolt12Invoice(_ context.Context,
	hash lntypes.Hash, invoice *invoices.Invoice) {

	m.invoices = append(m.invoices, invoice)
	m.hashes = append(m.hashes, hash)
}

// mockReplier captures reply invocations and returns err.
type mockReplier struct {
	replies [][]byte
	err     error
}

func (m *mockReplier) SendInvoiceReply(_ context.Context, invoiceBytes []byte,
	_ *sphinx.BlindedPath) error {

	m.replies = append(m.replies, invoiceBytes)

	return m.err
}

// addOffer creates a bolt12 offer and stores it in the mock store. It returns
// the codec offer and the stored record.
func addOffer(t *testing.T, store *mockOfferStore, nodeKey *btcec.PrivateKey,
	amountMsat uint64) (*bolt12.Offer, *offers.Offer) {

	t.Helper()

	b12Offer := testOffer(t, nodeKey, amountMsat)

	return b12Offer, storeOffer(t, store, b12Offer)
}

// storeOffer stores the codec offer in the mock store and returns the stored
// record.
func storeOffer(t *testing.T, store *mockOfferStore,
	b12Offer *bolt12.Offer) *offers.Offer {

	t.Helper()

	encoded, err := bolt12.EncodeOfferString(b12Offer)
	require.NoError(t, err)

	offerHash, err := bolt12.OfferHash(b12Offer)
	require.NoError(t, err)

	offer := &offers.Offer{
		Hash:    offerHash,
		Encoded: encoded,
	}

	_, err = store.InsertOffer(t.Context(), offer)
	require.NoError(t, err)

	return offer
}

// buildSignedInvreqBytes constructs a signed invoice request and returns the
// raw TLV bytes.
func buildSignedInvreqBytes(t *testing.T, offer *bolt12.Offer,
	payerKey *btcec.PrivateKey, invreqAmount uint64) []byte {

	t.Helper()

	ir := testInvoiceRequest(t, offer, payerKey, invreqAmount)

	sig, err := bolt12.SignInvoiceRequest(ir, payerKey)
	require.NoError(t, err)

	ir.Signature = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType240, [64]byte]{
			Val: sig,
		},
	)

	finalBytes, err := ir.EncodeSigned()
	require.NoError(t, err)

	return finalBytes
}

// TestHandleInvoiceRequestErrors verifies that the handler returns the error
// of each step that refuses a request. The reader checks the quantity and the
// amount, so one reader rule shows that the handler runs the reader.
func TestHandleInvoiceRequestErrors(t *testing.T) {
	t.Parallel()

	nodeKey := testKey(t)

	var payerSeed [32]byte
	payerSeed[0] = 0xFF
	payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

	errReply := errors.New("reply failed")

	now := time.Unix(4102444800, 0)

	tests := []struct {
		name         string
		invreqAmount uint64
		noReplyPath  bool
		unsigned     bool
		unstored     bool
		disabled     bool
		expired      bool
		replyErr     error
		wantErr      error
	}{
		{
			name:         "no reply path",
			invreqAmount: 10000,
			noReplyPath:  true,
			wantErr:      errNoReplyPath,
		},
		{
			name:         "reader rule",
			invreqAmount: 5000,
			unsigned:     true,
			wantErr:      bolt12.ErrAmountBelowExpected,
		},
		{
			name:         "offer not found",
			invreqAmount: 10000,
			unstored:     true,
			wantErr:      offers.ErrOfferNotFound,
		},
		{
			name:         "offer disabled",
			invreqAmount: 10000,
			disabled:     true,
			wantErr:      ErrOfferDisabled,
		},
		{
			name:         "offer expired",
			invreqAmount: 10000,
			expired:      true,
			wantErr:      bolt12.ErrOfferExpired,
		},
		{
			name:         "reply fails",
			invreqAmount: 10000,
			replyErr:     errReply,
			wantErr:      errReply,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newMockOfferStore()
			notifier := &mockNotifier{}
			handler, err := NewHandler(
				store, notifier,
				&mockReplier{err: tc.replyErr},
				newPrivKeySigner(nodeKey), nil,
				clock.NewTestClock(now), testChainHash(),
				testFinalCLTVDelta,
			)
			require.NoError(t, err)

			offer := testOffer(t, nodeKey, 10000)
			if tc.expired {
				expiry := bolt12.TUint64(now.Unix() - 1)
				offer.OfferAbsoluteExpiry = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType14](
						expiry,
					),
				)
			}
			if !tc.unstored {
				stored := storeOffer(t, store, offer)
				stored.IsDisabled = tc.disabled
			}

			ir := testInvoiceRequest(
				t, offer, payerKey, tc.invreqAmount,
			)

			// The codec refuses to sign a request that breaks a
			// reader rule, so encode the records directly. The
			// reader checks the signature after the amount, so a
			// placeholder signature reaches the amount rule.
			var sig [64]byte
			if !tc.unsigned {
				var err error
				sig, err = bolt12.SignInvoiceRequest(
					ir, payerKey,
				)
				require.NoError(t, err)
			}
			ir.Signature = tlv.SomeRecordT(
				tlv.RecordT[tlv.TlvType240, [64]byte]{
					Val: sig,
				},
			)

			var buf bytes.Buffer
			err = lnwire.EncodePureTLVMessage(ir, &buf)
			require.NoError(t, err)

			replyPath := &sphinx.BlindedPath{
				IntroductionPoint: nodeKey.PubKey(),
			}
			if tc.noReplyPath {
				replyPath = nil
			}
			err = handler.HandleInvoiceRequest(
				t.Context(), buf.Bytes(), replyPath,
			)
			require.ErrorIs(t, err, tc.wantErr)

			// A rejected request, or an invoice that could not be
			// sent, must not reach the subscribers.
			require.Empty(t, notifier.invoices)
		})
	}
}
