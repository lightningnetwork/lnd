package bolt12handler

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestGenerateInvoice_HappyPath verifies that a valid invoice is generated from
// a well-formed invoice request.
func TestGenerateInvoice_HappyPath(t *testing.T) {
	t.Parallel()

	nodeKey := testKey(t)
	offer := testOffer(t, nodeKey, 10000)

	var payerSeed [32]byte
	payerSeed[0] = 0xFF
	payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

	ir := testInvoiceRequest(t, offer, payerKey, 10000)

	result, err := GenerateInvoice(testInvoiceConfig(nodeKey), ir)
	require.NoError(t, err)

	require.NotEmpty(t, result.Encoded)
	require.NotEqual(t, lntypes.Preimage{}, result.Preimage)
	require.NotEqual(t, lntypes.Hash{}, result.PaymentHash)
	require.NotEqual(t, [32]byte{}, result.PathID)

	require.Equal(t, result.Preimage.Hash(), result.PaymentHash)

	// The string decodes through the reader gates, which include the
	// signature check.
	decoded, err := bolt12.DecodeInvoiceString(
		result.Encoded, time.Now(), testChainHash(),
	)
	require.NoError(t, err)

	// The payer checks the invoice against its own request and against
	// the issuer key of the offer.
	err = bolt12.ValidateInvoiceForPayment(
		decoded, ir, time.Now(), testChainHash(),
		bolt12.InvoiceKnownFeatures{
			Invoice: bolt12.Bolt12Features,
			Blinded: bolt12.Bolt12Features,
		},
		nodeKey.PubKey(),
	)
	require.NoError(t, err)

	require.EqualValues(t, 10000, decoded.InvoiceAmount.UnwrapOrFailV(t))
	require.EqualValues(
		t, testNow.Unix(), decoded.InvoiceCreatedAt.UnwrapOrFailV(t),
	)
	require.Equal(
		t, nodeKey.PubKey(), decoded.InvoiceNodeID.UnwrapOrFailV(t),
	)

	// Verify mirrored fields.
	require.Equal(
		t, "test offer",
		string(decoded.OfferDescription.UnwrapOrFailV(t)),
	)
	require.Equal(
		t, payerKey.PubKey(), decoded.InvreqPayerID.UnwrapOrFailV(t),
	)
}

// TestGenerateInvoiceAmount verifies that the invoice amount follows the
// precedence of computeInvoiceAmount.
func TestGenerateInvoiceAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		offerAmount  uint64
		invreqAmount uint64
		quantity     fn.Option[uint64]
		currency     string
		want         uint64
		wantErr      error
	}{
		{
			name:        "fixed offer amount",
			offerAmount: 10000,
			want:        10000,
		},
		{
			name:         "invreq amount above offer amount",
			offerAmount:  10000,
			invreqAmount: 15000,
			want:         15000,
		},
		{
			name:         "invreq amount without offer amount",
			invreqAmount: 25000,
			want:         25000,
		},
		{
			name:        "offer amount times quantity",
			offerAmount: 1000,
			quantity:    fn.Some[uint64](5),
			want:        5000,
		},
		{
			name:        "offer amount times quantity overflows",
			offerAmount: 1 << 63,
			quantity:    fn.Some[uint64](2),
			wantErr:     errAmountOverflow,
		},
		{
			name:         "explicit zero quantity",
			offerAmount:  1000,
			invreqAmount: 1000,
			quantity:     fn.Some[uint64](0),
			wantErr:      bolt12.ErrQuantityZero,
		},
		{
			name:        "currency offer amount without invreq",
			offerAmount: 5,
			currency:    "USD",
			wantErr:     errCurrencyConversion,
		},
		{
			name:    "no offer amount and no invreq amount",
			wantErr: bolt12.ErrMissingAmount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodeKey := testKey(t)
			offer := testOffer(t, nodeKey, tc.offerAmount)

			// The request copies the offer fields, so the quantity
			// cap must be on the offer before the request exists.
			// No validator runs on this fixture.
			if tc.quantity.IsSome() {
				setQuantityMax(offer, 10)
			}
			if tc.currency != "" {
				offer.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob(tc.currency),
					),
				)
			}

			var payerSeed [32]byte
			payerSeed[0] = 0xFF
			payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

			ir := testInvoiceRequest(
				t, offer, payerKey, tc.invreqAmount,
			)
			tc.quantity.WhenSome(func(q uint64) {
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType86](
						bolt12.TUint64(q),
					),
				)
			})

			result, err := GenerateInvoice(
				testInvoiceConfig(nodeKey), ir,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			decoded, err := bolt12.DecodeInvoiceString(
				result.Encoded, time.Now(), testChainHash(),
			)
			require.NoError(t, err)

			require.EqualValues(
				t, tc.want,
				decoded.InvoiceAmount.UnwrapOrFailV(t),
			)
		})
	}
}

// TestGenerateInvoice_BlindedPath verifies that the generated invoice includes
// a blinded payment path with a path_id.
func TestGenerateInvoice_BlindedPath(t *testing.T) {
	t.Parallel()

	nodeKey := testKey(t)
	offer := testOffer(t, nodeKey, 10000)

	var payerSeed [32]byte
	payerSeed[0] = 0xFF
	payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

	ir := testInvoiceRequest(t, offer, payerKey, 10000)

	result, err := GenerateInvoice(testInvoiceConfig(nodeKey), ir)
	require.NoError(t, err)

	decoded, err := bolt12.DecodeInvoiceString(
		result.Encoded, time.Now(), testChainHash(),
	)
	require.NoError(t, err)

	paths := decoded.InvoicePaths.UnwrapOrFailV(t).Paths
	require.Len(t, paths, 1)
	require.Len(t, paths[0].Hops, 1)

	// The single hop's encrypted data contains the path_id in encrypted
	// form.
	require.NotEmpty(t, paths[0].Hops[0].EncryptedData)

	require.Len(t, decoded.InvoiceBlindedPay.UnwrapOrFailV(t).Infos, 1)
}

// fakePathBuilder is a PaymentPathBuilder that returns a fixed result and
// records its arguments.
type fakePathBuilder struct {
	result *PaymentPathResult
	err    error

	gotAmount uint64
	gotPathID []byte
}

// BuildPaymentPaths returns the fixed result and error.
func (f *fakePathBuilder) BuildPaymentPaths(amountMsat uint64,
	pathID []byte) (*PaymentPathResult, error) {

	f.gotAmount = amountMsat
	f.gotPathID = pathID

	return f.result, f.err
}

// TestGenerateInvoicePathBuilder verifies that GenerateInvoice uses the paths
// of the builder, and uses the single-hop path when the builder fails or
// returns no paths.
func TestGenerateInvoicePathBuilder(t *testing.T) {
	t.Parallel()

	// A fee that the single-hop fallback never sets marks the builder's
	// pay info.
	const builderFee = 7

	tests := []struct {
		name         string
		builderErr   error
		noResult     bool
		noPaths      bool
		wantFeeBase  uint32
		wantFallback bool
	}{
		{
			name:        "builder paths",
			wantFeeBase: builderFee,
		},
		{
			name:         "builder error",
			builderErr:   errors.New("no route"),
			wantFallback: true,
		},
		{
			name:         "builder without result",
			noResult:     true,
			wantFallback: true,
		},
		{
			name:         "builder with zero paths",
			noPaths:      true,
			wantFallback: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodeKey := testKey(t)
			offer := testOffer(t, nodeKey, 10000)

			var payerSeed [32]byte
			payerSeed[0] = 0xFF
			payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

			ir := testInvoiceRequest(t, offer, payerKey, 0)

			path, err := buildSingleHopBlindedPath(
				nodeKey.PubKey(), []byte{1},
			)
			require.NoError(t, err)

			builder := &fakePathBuilder{err: tc.builderErr}
			if !tc.noResult {
				builder.result = &PaymentPathResult{
					Paths: []lnwire.BlindedPath{path},
					PayInfos: []bolt12.BlindedPayInfo{{
						FeeBaseMsat:     builderFee,
						HtlcMaximumMsat: 10000,
					}},
				}
			}
			if tc.noPaths {
				builder.result = &PaymentPathResult{}
			}

			cfg := testInvoiceConfig(nodeKey)
			cfg.PathBuilder = builder

			result, err := GenerateInvoice(cfg, ir)
			require.NoError(t, err)

			require.EqualValues(t, 10000, builder.gotAmount)
			require.Equal(t, result.PathID[:], builder.gotPathID)

			decoded, err := bolt12.DecodeInvoiceString(
				result.Encoded, time.Now(), testChainHash(),
			)
			require.NoError(t, err)

			payInfos := decoded.InvoiceBlindedPay.UnwrapOrFailV(t)
			infos := payInfos.Infos
			require.Len(t, infos, 1)
			require.Equal(t, tc.wantFeeBase, infos[0].FeeBaseMsat)

			if tc.wantFallback {
				wantDelta := testFinalCLTVDelta +
					routing.BlockPadding
				require.EqualValues(
					t, wantDelta, infos[0].CltvExpiryDelta,
				)
			}
		})
	}
}

// TestGenerateInvoiceIssuerID verifies that GenerateInvoice signs an invoice
// for an offer with offer_issuer_id, and refuses an offer with offer_paths and
// no offer_issuer_id, because the invoice would carry the node identity key.
func TestGenerateInvoiceIssuerID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		issuerID bool
		wantErr  error
	}{
		// lnd never creates an offer with both fields, but another
		// implementation can. The issuer id then binds the invoice to
		// the node key, and the paths only route the request.
		{
			name:     "offer paths with issuer id",
			issuerID: true,
		},
		{
			name:    "offer paths without issuer id",
			wantErr: errOfferWithoutIssuerID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodeKey := testKey(t)
			offer := testOffer(t, nodeKey, 10000)

			path, err := buildSingleHopBlindedPath(
				nodeKey.PubKey(), []byte{1},
			)
			require.NoError(t, err)

			offer.OfferPaths = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType16](
					lnwire.BlindedPaths{
						Paths: []lnwire.BlindedPath{
							path,
						},
					},
				),
			)
			if !tc.issuerID {
				offer.OfferIssuerID = tlv.OptionalRecordT[
					tlv.TlvType22, *btcec.PublicKey,
				]{}
			}

			var payerSeed [32]byte
			payerSeed[0] = 0xFF
			payerKey, _ := btcec.PrivKeyFromBytes(payerSeed[:])

			ir := testInvoiceRequest(t, offer, payerKey, 0)

			result, err := GenerateInvoice(
				testInvoiceConfig(nodeKey), ir,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			require.Equal(
				t, nodeKey.PubKey(),
				result.Invoice.InvoiceNodeID.UnwrapOrFailV(t),
			)
		})
	}
}
