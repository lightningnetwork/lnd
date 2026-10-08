//go:build test_db_sqlite || test_db_postgres

package offers

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
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

	// The subtests share one database server, and each gets its own
	// database.
	fixture := newTestFixture(t)

	intro, err := lnwire.NewPubkeyIntro(privKey.PubKey())
	require.NoError(t, err)
	path := lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    privKey.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: privKey.PubKey(),
			EncryptedData: []byte{1, 2, 3},
		}},
	}

	mainnet := [32]byte(*chaincfg.MainNetParams.GenesisHash)
	testnet := [32]byte(*chaincfg.TestNet3Params.GenesisHash)

	// manyChains returns n distinct chains, with mainnet first, so that
	// the stored offer decodes on mainnet.
	manyChains := func(n int) [][32]byte {
		chains := [][32]byte{mainnet}
		for i := 1; i < n; i++ {
			chains = append(chains, [32]byte{byte(i)})
		}

		return chains
	}
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
		wantErr: bolt12.ErrMissingDescription,
	}, {
		name: "missing issuer key",
		params: CreateOfferParams{
			Description: "test",
		},
		wantErr: bolt12.ErrNilPublicKey,
	}, {
		name: "empty offer paths",
		params: CreateOfferParams{
			Identity: fn.NewRight[*btcec.PublicKey](
				[]lnwire.BlindedPath{},
			),
			Description: "test",
		},
		wantErr: bolt12.ErrEmptyBlindedPaths,
	}, {
		name: "blinded paths",
		params: CreateOfferParams{
			Identity: fn.NewRight[*btcec.PublicKey](
				[]lnwire.BlindedPath{path},
			),
			Description: "private",
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			require.True(t, offer.OfferPaths.IsSome())
			require.True(t, offer.OfferIssuerID.IsNone())
		},
	}, {
		name: "chains",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "multi-chain",
			Chains:      [][32]byte{mainnet, testnet},
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			require.True(t, offer.OfferChains.IsSome())
		},
	}, {
		name: "chains at the decode limit",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "many chains",
			Chains:      manyChains(32),
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			chains := offer.OfferChains.UnwrapOrFailV(t).Chains
			require.Len(t, chains, 32)
		},
	}, {
		name: "chains above the decode limit",
		params: CreateOfferParams{
			Identity:    identity,
			Description: "too many chains",
			Chains:      manyChains(33),
		},
		wantErr: bolt12.ErrTooManyChains,
	}, {
		name: "description at the record size limit",
		params: CreateOfferParams{
			Identity:    identity,
			Description: strings.Repeat("a", tlv.MaxRecordSize),
		},
		check: func(t *testing.T, offer *bolt12.Offer) {
			desc := offer.OfferDescription.UnwrapOrFailV(t)
			require.Len(t, desc, tlv.MaxRecordSize)
		},
	}, {
		name: "description above the record size limit",
		params: CreateOfferParams{
			Identity: identity,
			Description: strings.Repeat(
				"a", tlv.MaxRecordSize+1,
			),
		},
		wantErr: tlv.ErrRecordTooLarge,
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

			store := newTestSQLStore(t, fixture)
			result, err := CreateOffer(
				t.Context(), store, testClock, tc.params,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			// The stored offer is the one CreateOffer returned.
			got, err := store.GetOfferByHash(
				t.Context(), result.Hash,
			)
			require.NoError(t, err)
			require.Equal(t, result.Encoded, got.Encoded)

			// An issuer key offer carries the key it was created
			// with.
			offer := decodeStoredOffer(t, store, result.Hash)
			if tc.params.Identity.IsLeft() {
				key := offer.OfferIssuerID.UnwrapOrFailV(t)
				require.True(t, privKey.PubKey().IsEqual(key))
				require.True(t, offer.OfferPaths.IsNone())
			}

			tc.check(t, offer)
		})
	}
}

// TestCreateOfferDuplicate verifies that creating the same offer twice fails,
// because the offer hash is unique.
func TestCreateOfferDuplicate(t *testing.T) {
	t.Parallel()

	store := newTestSQLStore(t, newTestFixture(t))
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
