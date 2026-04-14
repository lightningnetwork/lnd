package offers

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// issuerIdentity wraps a pubkey as a Left Either for CreateOfferParams.
func issuerIdentity(key *btcec.PublicKey) fn.Either[*btcec.PublicKey,
	[]lnwire.BlindedPath] {

	return fn.NewLeft[*btcec.PublicKey, []lnwire.BlindedPath](key)
}

// testIssuerKey generates a deterministic private key for testing.
func testIssuerKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	// Use a fixed seed for deterministic tests.
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	privKey, _ := btcec.PrivKeyFromBytes(seed[:])

	return privKey
}

// decodeStoredOffer looks up an offer by its offer hash and decodes the stored
// string.
func decodeStoredOffer(t *testing.T, store Store,
	offerHash [32]byte) *bolt12.Offer {

	t.Helper()

	got, err := store.GetOfferByHash(t.Context(), offerHash)
	require.NoError(t, err)

	// The time is before every expiry in these tests.
	decoded, err := bolt12.DecodeOfferString(
		got.Encoded, time.Unix(0, 0),
		[32]byte(*chaincfg.MainNetParams.GenesisHash),
	)
	require.NoError(t, err)

	return decoded
}

// TestCreateOffer verifies that CreateOffer validates its parameters, and
// that a created offer is stored and decodes with the requested fields.
func TestCreateOffer(t *testing.T) {
	t.Parallel()

	privKey := testIssuerKey(t)
	identity := issuerIdentity(privKey.PubKey())
	now := uint64(testTime.Unix())
	later := uint64(testTime.Add(time.Hour).Unix())

	testCases := []struct {
		name    string
		params  CreateOfferParams
		wantErr error

		// check inspects the decoded offer of a successful case.
		check func(t *testing.T, offer *bolt12.Offer)
	}{{
		name: "fixed amount",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "coffee",
			AmountMsat:  10000,
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			amount := offer.OfferAmount.UnwrapOrFailV(t)
			require.Equal(t, bolt12.TUint64(10000), amount)
			require.True(t, offer.OfferQuantityMax.IsNone())
			require.True(t, offer.OfferAbsoluteExpiry.IsNone())
		},
	}, {
		name: "any amount",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "tips",
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			require.True(t, offer.OfferAmount.IsNone())
		},
	}, {
		name: "amount without description",
		params: CreateOfferParams{
			Identity:   identity,
			AmountMsat: 10000,
		},
		wantErr: ErrMissingDescription,
	}, {
		name: "missing issuer key",
		params: CreateOfferParams{
			Description: "test",
		},
		wantErr: ErrMissingIssuerKey,
	}, {
		name: "expiry in the future",
		params: CreateOfferParams{
			Identity:       identity,
			Description:    "limited time",
			AmountMsat:     5000,
			AbsoluteExpiry: later,
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			expiry := offer.OfferAbsoluteExpiry.UnwrapOrFailV(t)
			require.Equal(t, bolt12.TUint64(later), expiry)
		},
	}, {
		name: "expiry before now",
		params: CreateOfferParams{
			Identity:       identity,
			Description:    "expired",
			AmountMsat:     5000,
			AbsoluteExpiry: now - 1,
		},
		wantErr: ErrExpiryNotInFuture,
	}, {
		name: "expiry at now",
		params: CreateOfferParams{
			Identity:       identity,
			Description:    "expired",
			AmountMsat:     5000,
			AbsoluteExpiry: now,
		},
		wantErr: ErrExpiryNotInFuture,
	}, {
		name: "quantity limit",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "stickers",
			AmountMsat:  1000,
			QuantityMax: fn.Some[uint64](10),
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			qty := offer.OfferQuantityMax.UnwrapOrFailV(t)
			require.Equal(t, bolt12.TUint64(10), qty)
		},
	}, {
		name: "unlimited quantity",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "stickers",
			AmountMsat:  1000,
			QuantityMax: fn.Some[uint64](0),
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			qty := offer.OfferQuantityMax.UnwrapOrFailV(t)
			require.Zero(t, qty)
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestSQLStore(t)
			result, err := CreateOffer(
				t.Context(), store, testClock, tc.params,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			// The stored offer is the one CreateOffer returned,
			// and it carries the issuer id it was created with.
			got, err := store.GetOfferByHash(
				t.Context(), result.Hash,
			)
			require.NoError(t, err)
			require.Equal(t, result.Encoded, got.Encoded)

			offer := decodeStoredOffer(t, store, result.Hash)
			issuer := offer.OfferIssuerID.UnwrapOrFailV(t)
			require.True(t, privKey.PubKey().IsEqual(issuer))

			tc.check(t, offer)
		})
	}
}

// TestCreateOfferDuplicate verifies that creating the same offer twice fails,
// because the offer hash is unique.
func TestCreateOfferDuplicate(t *testing.T) {
	t.Parallel()

	store := newTestSQLStore(t)
	ctx := t.Context()
	privKey := testIssuerKey(t)

	params := CreateOfferParams{
		Identity:    issuerIdentity(privKey.PubKey()),
		Description: "coffee",
		AmountMsat:  10000,
	}

	_, err := CreateOffer(ctx, store, testClock, params)
	require.NoError(t, err)

	// The same parameters produce the same offer hash.
	_, err = CreateOffer(ctx, store, testClock, params)
	require.ErrorIs(t, err, ErrOfferExists)
}
