package bolt12

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// validBobOffer is the spec-minimal happy-path offer that each table row
// mutates to isolate the rule under test.
func validBobOffer(t *testing.T) *Offer {
	t.Helper()

	_, pub := bobKey()

	return &Offer{
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](pub),
		),
	}
}

// TestValidateOfferWrite pins the BOLT 12 writer-side MUSTs that the codec can
// enforce.
func TestValidateOfferWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Offer)
		wantErr error
	}{
		{
			name:    "happy path with issuer_id only",
			mutate:  func(*Offer) {},
			wantErr: nil,
		},
		{
			name: "amount without description",
			mutate: func(o *Offer) {
				o.OfferAmount = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType8](
						TUint64(1000),
					),
				)
			},
			wantErr: ErrMissingDescription,
		},
		{
			name: "currency without amount",
			mutate: func(o *Offer) {
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6,
						tlv.Blob](tlv.Blob("USD")),
				)
			},
			wantErr: ErrCurrencyWithoutAmount,
		},
		{
			name: "zero amount with description",
			mutate: func(o *Offer) {
				o.OfferAmount = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType8](
						TUint64(0),
					),
				)
				o.OfferDescription = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10,
						tlv.Blob](tlv.Blob("a tip")),
				)
			},
			wantErr: ErrZeroAmount,
		},
		{
			name: "no issuer or paths",
			mutate: func(o *Offer) {
				o.OfferIssuerID = tlv.OptionalRecordT[
					tlv.TlvType22, *btcec.PublicKey]{}
			},
			wantErr: ErrNoIssuerIdentity,
		},
		{
			name: "empty offer_chains",
			mutate: func(o *Offer) {
				o.OfferChains = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType2](
						ChainsRecord{Chains: nil},
					),
				)
			},
			wantErr: ErrEmptyChains,
		},
		{
			name: "currency wrong length",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6,
						tlv.Blob](tlv.Blob("US")),
				)
			},
			wantErr: ErrInvalidCurrency,
		},
		{
			name: "currency unknown ISO 4217 code",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6,
						tlv.Blob](tlv.Blob("ZZZ")),
				)
			},
			wantErr: ErrInvalidCurrency,
		},
		{
			// Pins the docstring claim that ValidateOfferWrite's
			// offerAllowedRange loop exists to catch a
			// decoded-then-mutated offer with an out-of-range TLV
			// resurfacing via decodedTLVs.
			name: "out-of-range TLV in decoded extras",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{200: nil}
			},
			wantErr: ErrOutOfRangeType,
		},
		{
			name: "empty blinded paths list",
			mutate: func(o *Offer) {
				o.OfferPaths = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType16](
						lnwire.BlindedPaths{Paths: nil},
					),
				)
			},
			wantErr: ErrEmptyBlindedPaths,
		},
		{
			name: "blinded path with zero hops",
			mutate: func(o *Offer) {
				_, intro := aliceKey()
				_, blinding := bobKey()
				pk := lnwire.PubkeyIntro{Pubkey: intro}
				o.OfferPaths = tlv.SomeRecordT(
					//nolint:ll
					tlv.NewRecordT[tlv.TlvType16](
						lnwire.BlindedPaths{
							Paths: []lnwire.BlindedPath{{
								IntroductionNode: pk,
								BlindingPoint:    blinding,
								Hops:             nil,
							}},
						},
					),
				)
			},
			wantErr: lnwire.ErrEmptyBlindedPath,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := validBobOffer(t)
			tc.mutate(o)

			err := ValidateOfferWrite(o)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestValidateOfferRead pins the BOLT 12 reader-side MUSTs so a malformed or
// unsafe offer is rejected before any invoice request reaches the wire.
func TestValidateOfferRead(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)

	var nonBitcoin [32]byte
	nonBitcoin[0] = 0x01

	tests := []struct {
		name        string
		mutate      func(*Offer)
		activeChain [32]byte
		wantErr     error
	}{
		{
			name:        "happy path on bitcoin mainnet",
			mutate:      func(*Offer) {},
			activeChain: bitcoinMainnetGenesisHash,
		},
		{
			name: "out-of-range TLV in decoded extras",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{200: nil}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "unknown even TLV type in range rejected",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{24: nil}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrUnknownEvenType,
		},
		{
			name: "unknown even feature bit rejected",
			mutate: func(o *Offer) {
				o.OfferFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType12](
						*lnwire.NewRawFeatureVector(0),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrUnknownEvenFeature,
		},
		{
			name: "unknown odd feature bit ignored",
			mutate: func(o *Offer) {
				o.OfferFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType12](
						*lnwire.NewRawFeatureVector(1),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "non-bitcoin chain rejected when " +
				"offer_chains absent",
			mutate:      func(*Offer) {},
			activeChain: nonBitcoin,
			wantErr:     ErrUnsupportedChain,
		},
		{
			name: "explicit chain list missing active chain",
			mutate: func(o *Offer) {
				var c [32]byte
				c[0] = 0xaa
				o.OfferChains = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType2](
						ChainsRecord{
							Chains: [][32]byte{c},
						},
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrUnsupportedChain,
		},
		{
			name: "empty offer_chains list",
			mutate: func(o *Offer) {
				o.OfferChains = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType2](
						ChainsRecord{Chains: nil},
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrEmptyChains,
		},
		{
			name: "amount without description",
			mutate: func(o *Offer) {
				o.OfferAmount = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType8](
						TUint64(1000),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrMissingDescription,
		},
		{
			name: "currency without amount",
			mutate: func(o *Offer) {
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6,
						tlv.Blob](tlv.Blob("USD")),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrCurrencyWithoutAmount,
		},
		{
			name: "zero amount with description",
			mutate: func(o *Offer) {
				o.OfferAmount = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType8](
						TUint64(0),
					),
				)
				o.OfferDescription = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10,
						tlv.Blob](tlv.Blob("a tip")),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrZeroAmount,
		},
		{
			name: "missing issuer and paths",
			mutate: func(o *Offer) {
				o.OfferIssuerID = tlv.OptionalRecordT[
					tlv.TlvType22, *btcec.PublicKey]{}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrNoIssuerIdentity,
		},
		{
			name: "blinded path with zero hops",
			mutate: func(o *Offer) {
				_, intro := aliceKey()
				_, blinding := bobKey()
				pk := lnwire.PubkeyIntro{Pubkey: intro}
				o.OfferPaths = tlv.SomeRecordT(
					//nolint:ll
					tlv.NewRecordT[tlv.TlvType16](
						lnwire.BlindedPaths{
							Paths: []lnwire.BlindedPath{{
								IntroductionNode: pk,
								BlindingPoint:    blinding,
								Hops:             nil,
							}},
						},
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     lnwire.ErrEmptyBlindedPath,
		},
		{
			name: "expired offer",
			mutate: func(o *Offer) {
				expiry := uint64(now.Unix()) - 1
				o.OfferAbsoluteExpiry = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType14](
						TUint64(expiry),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOfferExpired,
		},
		{
			name: "currency wrong length",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6,
						tlv.Blob](tlv.Blob("US")),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrInvalidCurrency,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := validBobOffer(t)
			tc.mutate(o)

			err := ValidateOfferRead(o, now, tc.activeChain)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// addAmountAndDescription satisfies the dependency rules so currency-shape rows
// are not short-circuited before the ISO 4217 check runs.
func addAmountAndDescription(o *Offer) {
	o.OfferAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType8](TUint64(1000)),
	)
	o.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10, tlv.Blob](
			tlv.Blob("a tip"),
		),
	)
}
