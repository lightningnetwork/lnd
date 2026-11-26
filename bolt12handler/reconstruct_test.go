package bolt12handler

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testOfferPrice is the offer_amount of the offer of newTestReconstructor.
const testOfferPrice = 1000

// newTestReconstructor creates a Reconstructor with the path id key of the
// test node key and a mock offer store that holds one offer with the price
// testOfferPrice.
func newTestReconstructor(t *testing.T) (*Reconstructor,
	*btcec.PrivateKey, *offers.Offer) {

	t.Helper()

	nodeKey := testKey(t)
	store := newMockOfferStore()

	decoded := testOffer(t, nodeKey, testOfferPrice)
	encoded, err := bolt12.EncodeOfferString(decoded)
	require.NoError(t, err)

	offerHash, err := bolt12.OfferHash(decoded)
	require.NoError(t, err)

	offer := &offers.Offer{
		ID:      1,
		Hash:    offerHash,
		Encoded: encoded,
	}
	store.offers[offer.Hash] = offer

	r := NewReconstructor(
		testPathIDKey(nodeKey), store, testChainHash(),
		testFinalCLTVDelta,
	)

	return r, nodeKey, offer
}

// testEnvelope is an invoice envelope with the payment path id and the
// payment hash that the node made for it.
type testEnvelope struct {
	envelope    []byte
	pathID      chainhash.Hash
	paymentHash lntypes.Hash
	preimage    lntypes.Preimage
}

// buildTestEnvelope encodes an envelope with the given fields and makes its
// payment path id with the path id key of nodeKey.
func buildTestEnvelope(t *testing.T, nodeKey *btcec.PrivateKey,
	data *envelope.Data) *testEnvelope {

	t.Helper()

	if data.PayerID == ([33]byte{}) {
		payerKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		data.PayerID = [33]byte(payerKey.PubKey().SerializeCompressed())
	}

	envelope, err := envelope.Encode(data)
	require.NoError(t, err)

	key := testPathIDKey(nodeKey)
	binding := EnvelopeBinding(envelope)
	pathID, err := key.newPaymentPathID(binding)
	require.NoError(t, err)

	preimage, ok := key.checkPaymentPathID(pathID[:], binding)
	require.True(t, ok)

	return &testEnvelope{
		envelope:    envelope,
		pathID:      chainhash.Hash(pathID),
		paymentHash: preimage.Hash(),
		preimage:    preimage,
	}
}

// reconstruct returns the invoice that the reconstructor rebuilds from the
// envelope, or the error that it gives for it.
func (e *testEnvelope) reconstruct(t *testing.T,
	r *Reconstructor) (*invoices.Invoice, error) {

	return r.ReconstructInvoice(
		t.Context(), e.envelope, e.pathID, e.paymentHash,
	)
}

// TestReconstruct_Valid verifies that a valid envelope reconstructs a correct
// invoice.
func TestReconstruct_Valid(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some(offer.Hash),
		CreatedAt: uint64(time.Now().Unix()),
		Amount:    50000,
		Quantity:  3,
	})

	inv, err := env.reconstruct(t, r)
	require.NoError(t, err)

	require.True(t, inv.IsBolt12)
	require.Equal(t, fn.Some(offer.ID), inv.OfferID)
	require.Equal(t, offer.Hash[:], inv.OfferHash)
	require.Equal(t, [32]byte(env.pathID), inv.Terms.PaymentAddr)
	require.Equal(t, env.preimage, *inv.Terms.PaymentPreimage)
	require.Equal(t, env.envelope, inv.Bolt12Envelope)
	require.EqualValues(t, 50000, inv.Terms.Value)
	require.EqualValues(t, 3, inv.InvreqQuantity)
	require.EqualValues(t, testFinalCLTVDelta, inv.Terms.FinalCltvDelta)

	// The rebuilt invoice keeps the basic MPP that the issued invoice
	// advertised, with the features that it depends on.
	require.Equal(
		t, lnwire.NewRawFeatureVector(
			lnwire.MPPOptional, lnwire.PaymentAddrRequired,
			lnwire.TLVOnionPayloadRequired,
		),
		inv.Terms.Features.RawFeatureVector,
	)
}

// TestReconstruct_ForeignPathID verifies that a payment whose path id this
// node did not make for the envelope reconstructs nothing.
func TestReconstruct_ForeignPathID(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)

	// Each subtest gets its own copy, because buildTestEnvelope sets the
	// payer id.
	data := func() *envelope.Data {
		return &envelope.Data{
			OfferHash: fn.Some(offer.Hash),
			CreatedAt: uint64(time.Now().Unix()),
			Amount:    testOfferPrice,
		}
	}

	t.Run("changed envelope", func(t *testing.T) {
		t.Parallel()

		env := buildTestEnvelope(t, nodeKey, data())
		env.envelope[len(env.envelope)-1] ^= 0x01

		_, err := env.reconstruct(t, r)
		require.ErrorIs(t, err, errForeignPathID)
	})

	t.Run("path id of another node", func(t *testing.T) {
		t.Parallel()

		otherKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		env := buildTestEnvelope(t, otherKey, data())

		_, err = env.reconstruct(t, r)
		require.ErrorIs(t, err, errForeignPathID)
	})
}

// TestReconstruct_NoOffer verifies that an envelope that names no offer is not
// rebuilt. An invoice that answers no offer is stored when it is made.
func TestReconstruct_NoOffer(t *testing.T) {
	t.Parallel()

	r, nodeKey, _ := newTestReconstructor(t)

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		CreatedAt: uint64(time.Now().Unix()),
		Amount:    testOfferPrice,
	})

	_, err := env.reconstruct(t, r)
	require.ErrorIs(t, err, errNoOfferInEnvelope)
}

// TestReconstruct_HashMismatch verifies that reconstruction fails when the
// payment hash does not match the preimage of the path id.
func TestReconstruct_HashMismatch(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some(offer.Hash),
		CreatedAt: uint64(time.Now().Unix()),
		Amount:    2000,
	})
	env.paymentHash = lntypes.Hash{0xFF, 0xFF}

	_, err := env.reconstruct(t, r)
	require.ErrorContains(t, err, "preimage hash mismatch")
}

// TestReconstruct_Expired verifies that reconstruction fails for envelopes
// older than createdAt + bolt12.DefaultInvoiceRelativeExpiry.
func TestReconstruct_Expired(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)

	// Three hours ago is well past the default invoice expiry.
	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some(offer.Hash),
		CreatedAt: uint64(time.Now().Unix()) - 3*3600,
		Amount:    3000,
	})

	_, err := env.reconstruct(t, r)
	require.ErrorContains(t, err, "envelope expired")
}

// TestReconstruct_UnknownOffer verifies that reconstruction fails when the
// offer hash in the envelope does not match any stored offer.
func TestReconstruct_UnknownOffer(t *testing.T) {
	t.Parallel()

	r, nodeKey, _ := newTestReconstructor(t)

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some([32]byte{0xFF, 0xFE, 0xFD}),
		CreatedAt: uint64(time.Now().Unix()),
		Amount:    4000,
	})

	_, err := env.reconstruct(t, r)
	require.ErrorContains(t, err, "lookup offer")
}

// TestReconstruct_OfferPrice verifies that an envelope amount below the offer
// price does not settle. Only a forged envelope can carry such an amount.
func TestReconstruct_OfferPrice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		price    uint64
		currency string
		amount   uint64
		quantity uint64
		wantErr  error
	}{
		{
			name:    "amount below the price",
			price:   testOfferPrice,
			amount:  testOfferPrice - 1,
			wantErr: errAmountBelowOffer,
		},
		{
			name:   "amount equal to the price",
			price:  testOfferPrice,
			amount: testOfferPrice,
		},
		{
			name:   "amount above the price",
			price:  testOfferPrice,
			amount: testOfferPrice + 1,
		},
		{
			name:     "amount below the price of the quantity",
			price:    testOfferPrice,
			amount:   2*testOfferPrice + 1,
			quantity: 3,
			wantErr:  errAmountBelowOffer,
		},
		{
			name:   "offer without a price",
			amount: 1,
		},
		{
			name:     "offer priced in a currency",
			price:    testOfferPrice,
			currency: "USD",
			amount:   1,
			wantErr:  errCurrencyConversion,
		},
		{
			name:     "overflowing quantity",
			price:    testOfferPrice,
			amount:   math.MaxUint64,
			quantity: math.MaxUint64,
			wantErr:  errAmountOverflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, nodeKey, offer := newTestReconstructor(t)

			decoded := testOffer(t, nodeKey, tc.price)
			if tc.currency != "" {
				decoded.OfferCurrency = tlv.SomeRecordT(
					tlv.RecordT[tlv.TlvType6, tlv.Blob]{
						Val: []byte(tc.currency),
					},
				)
			}
			encoded, err := bolt12.EncodeOfferString(decoded)
			require.NoError(t, err)
			offer.Encoded = encoded

			env := buildTestEnvelope(
				t, nodeKey, &envelope.Data{
					OfferHash: fn.Some(offer.Hash),
					CreatedAt: uint64(time.Now().Unix()),
					Amount:    tc.amount,
					Quantity:  tc.quantity,
				},
			)

			_, err = env.reconstruct(t, r)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
		})
	}
}

// TestReconstruct_Memo verifies that the reconstructed invoice carries the
// offer_description as its memo, cut to invoices.MaxMemoSize at a UTF-8 rune
// boundary.
func TestReconstruct_Memo(t *testing.T) {
	t.Parallel()

	// The two-byte rune starts at the last byte that fits, so a cut at
	// the size limit would split it.
	short := strings.Repeat("a", invoices.MaxMemoSize-1)
	long := short + "é" + "tail"

	tests := []struct {
		name        string
		description string
		wantMemo    string
	}{
		{
			name:        "description",
			description: "coffee",
			wantMemo:    "coffee",
		},
		{
			name:        "description above the size limit",
			description: long,
			wantMemo:    short,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, nodeKey, offer := newTestReconstructor(t)

			decoded := testOffer(t, nodeKey, 1000)
			decoded.OfferDescription = tlv.SomeRecordT(
				tlv.RecordT[tlv.TlvType10, tlv.Blob]{
					Val: []byte(tc.description),
				},
			)

			encoded, err := bolt12.EncodeOfferString(decoded)
			require.NoError(t, err)
			offer.Encoded = encoded

			env := buildTestEnvelope(
				t, nodeKey, &envelope.Data{
					OfferHash: fn.Some(offer.Hash),
					CreatedAt: uint64(time.Now().Unix()),
					Amount:    testOfferPrice,
				},
			)

			inv, err := env.reconstruct(t, r)
			require.NoError(t, err)
			require.Equal(t, tc.wantMemo, string(inv.Memo))
			require.NoError(
				t, invoices.ValidateInvoice(
					inv, env.paymentHash,
				),
			)
		})
	}
}

// TestReconstruct_OfferExpiredLater verifies that an offer that expired after
// the invoice was issued does not fail the payment of that invoice.
func TestReconstruct_OfferExpiredLater(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)
	createdAt := uint64(time.Now().Unix()) - 100

	decoded := testOffer(t, nodeKey, testOfferPrice)
	decoded.OfferAbsoluteExpiry = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType14, bolt12.TUint64]{
			Val: bolt12.TUint64(createdAt + 1),
		},
	)
	encoded, err := bolt12.EncodeOfferString(decoded)
	require.NoError(t, err)
	offer.Encoded = encoded

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some(offer.Hash),
		CreatedAt: createdAt,
		Amount:    testOfferPrice,
	})

	_, err = env.reconstruct(t, r)
	require.NoError(t, err)
}

// TestCheckOfferActive verifies that CheckOfferActive refuses an offer hash
// that is malformed, unknown or disabled, and accepts an active offer.
func TestCheckOfferActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hash     []byte
		disabled bool
		wantErr  string
	}{
		{
			name: "active offer",
		},
		{
			name:     "disabled offer",
			disabled: true,
			wantErr:  ErrOfferDisabled.Error(),
		},
		{
			name:    "unknown offer",
			hash:    make([]byte, 32),
			wantErr: offers.ErrOfferNotFound.Error(),
		},
		{
			name:    "short offer hash",
			hash:    make([]byte, 31),
			wantErr: "invalid offer hash length",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, _, offer := newTestReconstructor(t)
			offer.IsDisabled = tc.disabled

			hash := tc.hash
			if hash == nil {
				hash = offer.Hash[:]
			}

			err := r.CheckOfferActive(t.Context(), hash)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
