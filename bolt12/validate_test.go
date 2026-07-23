package bolt12

import (
	"math"
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
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob("USD"),
					),
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
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						tlv.Blob("a tip"),
					),
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
			name: "present-but-nil issuer_id",
			mutate: func(o *Offer) {
				o.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "empty offer_chains",
			mutate: func(o *Offer) {
				o.OfferChains = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType2](
						ChainsRecord{
							Chains: nil,
						},
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
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob("US"),
					),
				)
			},
			wantErr: ErrInvalidCurrency,
		},
		{
			name: "currency unknown ISO 4217 code",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferCurrency = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob("ZZZ"),
					),
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
				o.decodedTLVs = tlv.TypeMap{
					200: nil,
				}
			},
			wantErr: ErrOutOfRangeType,
		},
		{
			name: "empty blinded paths list",
			mutate: func(o *Offer) {
				o.OfferPaths = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType16](
						lnwire.BlindedPaths{
							Paths: nil,
						},
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
				pk := lnwire.PubkeyIntro{
					Pubkey: intro,
				}
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
		{
			name: "invalid UTF-8 in description",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferDescription = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						tlv.Blob("\xff\xff"),
					),
				)
			},
			wantErr: ErrInvalidUTF8,
		},
		{
			name: "invalid UTF-8 in issuer",
			mutate: func(o *Offer) {
				o.OfferIssuer = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType18](
						tlv.Blob("\xff\x00"),
					),
				)
			},
			wantErr: ErrInvalidUTF8,
		},
	}

	for _, tc := range tests {
		t.Run(
			tc.name,
			func(t *testing.T) {
				t.Parallel()

				o := validBobOffer(t)
				tc.mutate(o)

				err := ValidateOfferWrite(o)
				if tc.wantErr == nil {
					require.NoError(t, err)

					return
				}
				require.ErrorIs(t, err, tc.wantErr)
			},
		)
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
		known       map[lnwire.FeatureBit]string
		wantErr     error
	}{
		{
			name:        "happy path on bitcoin mainnet",
			mutate:      func(*Offer) {},
			activeChain: bitcoinMainnetGenesisHash,
		},
		{
			name: "present-but-nil offer_issuer_id",
			mutate: func(o *Offer) {
				o.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrNilPublicKey,
		},
		{
			name: "out-of-range TLV in decoded extras",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					200: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "unknown even TLV type in range rejected",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					24: nil,
				}
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
						ChainsRecord{
							Chains: nil,
						},
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
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob("USD"),
					),
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
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						tlv.Blob("a tip"),
					),
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
				pk := lnwire.PubkeyIntro{
					Pubkey: intro,
				}
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
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tlv.Blob("US"),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrInvalidCurrency,
		},
		{
			name: "TLV type boundary 0 - out of range",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					0: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "TLV type boundary 1 - valid and ignored (odd)",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					1: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "TLV type boundary 79 - valid and ignored (odd)",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					79: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "TLV type boundary 80 - out of range",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					80: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "TLV type boundary 999999999 - out of range",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					999999999: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "TLV type boundary 1000000000 - even and " +
				"rejected",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					1000000000: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrUnknownEvenType,
		},
		{
			name: "TLV type boundary 1000000001 - valid and " +
				"ignored (odd)",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					1000000001: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "TLV type boundary 1999999999 - valid and " +
				"ignored (odd)",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					1999999999: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "TLV type boundary 2000000000 - out of range",
			mutate: func(o *Offer) {
				o.decodedTLVs = tlv.TypeMap{
					2000000000: nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrOutOfRangeType,
		},
		{
			name: "invalid UTF-8 in description",
			mutate: func(o *Offer) {
				addAmountAndDescription(o)
				o.OfferDescription = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						tlv.Blob("\xff\xff"),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrInvalidUTF8,
		},
		{
			name: "invalid UTF-8 in issuer",
			mutate: func(o *Offer) {
				o.OfferIssuer = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType18](
						tlv.Blob("\xff\x00"),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrInvalidUTF8,
		},
		{
			name: "quantity max = 0 (unlimited)",
			mutate: func(o *Offer) {
				o.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(0),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "quantity max > 0 (e.g. 5)",
			mutate: func(o *Offer) {
				o.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(5),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "now == expiry boundary (valid)",
			mutate: func(o *Offer) {
				expiry := uint64(now.Unix())
				o.OfferAbsoluteExpiry = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType14](
						TUint64(expiry),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "symmetric explicit bitcoin chain list " +
				"(inverted-default invariant)",
			mutate: func(o *Offer) {
				o.OfferChains = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType2](
						ChainsRecord{
							//nolint:ll
							Chains: [][32]byte{
								bitcoinMainnetGenesisHash,
							},
						},
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     nil,
		},
		{
			name: "multi-error determinism (sortedTypes contract " +
				"returning first sorted error)",
			mutate: func(o *Offer) {
				// 24 is unknown even type (in range) -> returns
				// ErrUnknownEvenType 200 is out of range type
				// -> returns ErrOutOfRangeType Since 24 is
				// sorted before 200, we must return
				// ErrUnknownEvenType.
				o.decodedTLVs = tlv.TypeMap{
					200: nil,
					24:  nil,
				}
			},
			activeChain: bitcoinMainnetGenesisHash,
			wantErr:     ErrUnknownEvenType,
		},
		{
			name: "known even feature bit accepted",
			mutate: func(o *Offer) {
				o.OfferFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType12](
						*lnwire.NewRawFeatureVector(0),
					),
				)
			},
			activeChain: bitcoinMainnetGenesisHash,
			known: map[lnwire.FeatureBit]string{
				0: "test_feature",
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(
			tc.name,
			func(t *testing.T) {
				t.Parallel()

				o := validBobOffer(t)
				tc.mutate(o)

				err := ValidateOfferRead(
					o, now, tc.activeChain, tc.known,
				)
				if tc.wantErr == nil {
					require.NoError(t, err)

					return
				}
				require.ErrorIs(t, err, tc.wantErr)
			},
		)
	}
}

// addAmountAndDescription satisfies the dependency rules so currency-shape rows
// are not short-circuited before the ISO 4217 check runs.
func addAmountAndDescription(o *Offer) {
	o.OfferAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType8](
			TUint64(1000),
		),
	)
	o.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10](
			tlv.Blob("a tip"),
		),
	)
}

// validInvoiceRequest is the spec-minimal happy-path invoice request that
// each table row mutates to isolate the rule under test.
func validInvoiceRequest(t *testing.T) *InvoiceRequest {
	t.Helper()

	ir := &InvoiceRequest{}

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ir.InvreqPayerID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType88](privKey.PubKey()),
	)

	ir.InvreqMetadata = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType0](
			[]byte("metadata"),
		),
	)

	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
	)

	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240]([64]byte{0x01}),
	)

	return ir
}

// TestValidateInvoiceRequestWrite pins the BOLT 12 writer-side MUSTs so a
// malformed or incomplete invoice request is rejected.
func TestValidateInvoiceRequestWrite(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	payerID := privKey.PubKey()

	tests := []struct {
		name    string
		mutate  func(*InvoiceRequest)
		wantErr error
	}{
		{
			name: "missing payer_id",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.OptionalRecordT[
					tlv.TlvType88, *btcec.PublicKey]{}
			},
			wantErr: ErrMissingPayerID,
		},
		{
			name: "present-but-nil payer_id",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "present-but-nil offer_issuer_id",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "missing description",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferDescription = tlv.OptionalRecordT[
					tlv.TlvType10, tlv.Blob]{}
			},
			wantErr: ErrMissingDescription,
		},
		{
			name: "missing metadata",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqMetadata = tlv.OptionalRecordT[
					tlv.TlvType0, tlv.Blob]{}
			},
			wantErr: ErrMissingMetadata,
		},
		{
			name: "missing amount",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqAmount = tlv.OptionalRecordT[
					tlv.TlvType82, TUint64]{}
			},
			wantErr: ErrMissingAmount,
		},
		{
			name: "invalid UTF-8 in payer_note",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPayerNote = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType89](
						[]byte{0xff},
					),
				)
			},
			wantErr: ErrInvalidUTF8,
		},
		{
			name: "empty blinded paths",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPaths = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType90](
						lnwire.BlindedPaths{Paths: nil},
					),
				)
			},
			wantErr: ErrEmptyBlindedPaths,
		},
		{
			name:   "happy path",
			mutate: func(*InvoiceRequest) {},
		},
		{
			name: "spontaneous request carrying quantity",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType86](
						TUint64(1),
					),
				)
			},
			wantErr: ErrQuantityWithoutMax,
		},
		{
			name: "spontaneous request offer quantity max",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(10),
					),
				)
			},
			wantErr: ErrOfferFieldsOnSpontaneous,
		},
		{
			name: "spontaneous request offer quantity max dup",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(10),
					),
				)
			},
			wantErr: ErrOfferFieldsOnSpontaneous,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ir := &InvoiceRequest{
				OfferDescription: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						tlv.Blob("description"),
					),
				),
				InvreqPayerID: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						payerID,
					),
				),
				InvreqMetadata: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType0](
						[]byte("metadata"),
					),
				),
				InvreqAmount: tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType82, TUint64](
						1000,
					),
				),
			}

			tc.mutate(ir)

			err := ValidateInvoiceRequestWrite(ir)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestValidateInvoiceRequestWriteAmountConstraints tests the writer constraints
// on invreq_amount.
func TestValidateInvoiceRequestWriteAmountConstraints(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	baseRequest := func() *InvoiceRequest {
		return &InvoiceRequest{
			OfferDescription: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType10](
					tlv.Blob("description"),
				),
			),
			InvreqPayerID: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType88](
					privKey.PubKey(),
				),
			),
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
		}
	}

	// 1. Spontaneous request (not responding to an offer):
	// - MUST set invreq_amount.
	t.Run("spontaneous_amount_required", func(t *testing.T) {
		ir := baseRequest()

		// Absent invreq_amount -> invalid.
		err := ValidateInvoiceRequestWrite(ir)
		require.ErrorIs(t, err, ErrMissingAmount)

		// Present invreq_amount -> valid.
		ir.InvreqAmount = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
		)
		require.NoError(t, ValidateInvoiceRequestWrite(ir))
	})

	// 2. Responding to an offer.
	t.Run("response_to_offer", func(t *testing.T) {
		baseResponseRequest := func() *InvoiceRequest {
			ir := baseRequest()
			ir.OfferIssuerID = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType22](
					privKey.PubKey(),
				),
			)

			return ir
		}

		// Case A: OfferAmount is absent.
		// - MUST specify invreq_amount.
		t.Run("offer_amount_absent", func(t *testing.T) {
			ir := baseResponseRequest()

			// InvreqAmount absent -> invalid.
			err := ValidateInvoiceRequestWrite(ir)
			require.ErrorIs(t, err, ErrMissingAmount)

			// InvreqAmount present -> valid.
			ir.InvreqAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))
		})

		// Case B: OfferAmount present, OfferCurrency absent (Bitcoin).
		t.Run("offer_amount_present_bitcoin", func(t *testing.T) {
			ir := baseResponseRequest()
			ir.OfferAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType8, TUint64](1000),
			)
			ir.OfferQuantityMax = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType20](TUint64(10)),
			)
			ir.InvreqQuantity = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType86, TUint64](2),
			)

			// InvreqAmount is optional (MAY omit it).
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// If set, it MUST be >= OfferAmount * Quantity
			// (1000 * 2 = 2000). InvreqAmount < expected ->
			// invalid.
			ir.InvreqAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](1999),
			)
			err := ValidateInvoiceRequestWrite(ir)
			require.ErrorIs(t, err, ErrAmountBelowExpected)

			// InvreqAmount >= expected -> valid.
			ir.InvreqAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](2000),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))
		})

		// Case C: OfferAmount present, OfferCurrency present
		// (non-Bitcoin).
		t.Run("offer_amount_present_non_bitcoin", func(t *testing.T) {
			ir := baseResponseRequest()
			ir.OfferAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType8, TUint64](1000),
			)
			ir.OfferCurrency = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType6](
					tlv.Blob("USD"),
				),
			)
			ir.OfferQuantityMax = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType20](TUint64(10)),
			)
			ir.InvreqQuantity = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType86, TUint64](2),
			)

			// InvreqAmount < OfferAmount * Quantity is allowed
			// because currency conversion is checked dynamically
			// at runtime, not statically inside
			// ValidateInvoiceRequestWrite.
			ir.InvreqAmount = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](100),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))
		})
	})
}

// TestValidateInvoiceRequestWriteChainConstraints tests the writer constraints
// on invreq_chain.
func TestValidateInvoiceRequestWriteChainConstraints(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	testnetHash := [32]byte{1}
	regtestHash := [32]byte{2}

	// 1. Not responding to an offer: any invreq_chain is accepted.
	t.Run("spontaneous_any_chain_accepted", func(t *testing.T) {
		ir := &InvoiceRequest{
			OfferDescription: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType10](
					tlv.Blob("description"),
				),
			),
			InvreqPayerID: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType88](
					privKey.PubKey(),
				),
			),
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
			InvreqAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
			),
		}

		// Absent chain is OK.
		require.NoError(t, ValidateInvoiceRequestWrite(ir))

		// Bitcoin chain is OK.
		ir.InvreqChain = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80](
				bitcoinMainnetGenesisHash,
			),
		)
		require.NoError(t, ValidateInvoiceRequestWrite(ir))

		// Non-bitcoin chain is OK.
		ir.InvreqChain = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80](testnetHash),
		)
		require.NoError(t, ValidateInvoiceRequestWrite(ir))
	})

	// 2. Responding to an offer.
	t.Run("response_to_offer", func(t *testing.T) {
		baseRequest := func() *InvoiceRequest {
			return &InvoiceRequest{
				OfferIssuerID: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						privKey.PubKey(),
					),
				),
				InvreqPayerID: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						privKey.PubKey(),
					),
				),
				InvreqMetadata: tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType0](
						[]byte("metadata"),
					),
				),
				InvreqAmount: tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType82, TUint64](
						1000,
					),
				),
			}
		}

		// Case A: OfferChains is absent (defaults to Bitcoin mainnet).
		t.Run("offer_chains_absent", func(t *testing.T) {
			ir := baseRequest()

			// InvreqChain absent (valid, defaults to bitcoin).
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// InvreqChain == bitcoin (valid).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					bitcoinMainnetGenesisHash,
				),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// InvreqChain != bitcoin (invalid).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					testnetHash,
				),
			)
			require.ErrorIs(
				t, ValidateInvoiceRequestWrite(ir),
				ErrUnsupportedChain,
			)
		})

		// Case B: OfferChains is present.
		t.Run("offer_chains_present", func(t *testing.T) {
			// Sub-case B1: OfferChains contains only Bitcoin.
			ir := baseRequest()
			ir.OfferChains = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
					Chains: [][32]byte{
						bitcoinMainnetGenesisHash,
					},
				}),
			)

			// InvreqChain absent (valid, defaults to bitcoin).
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// InvreqChain == bitcoin (valid).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					bitcoinMainnetGenesisHash,
				),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// InvreqChain == testnet (invalid, not in offer
			// chains).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					testnetHash,
				),
			)
			require.ErrorIs(
				t, ValidateInvoiceRequestWrite(ir),
				ErrUnsupportedChain,
			)

			// Sub-case B2: OfferChains contains only Testnet
			// (not Bitcoin).
			ir = baseRequest()
			ir.OfferChains = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
					Chains: [][32]byte{testnetHash},
				}),
			)

			// InvreqChain absent (invalid, defaults to bitcoin
			// which is not in offer chains).
			require.ErrorIs(
				t, ValidateInvoiceRequestWrite(ir),
				ErrUnsupportedChain,
			)

			// InvreqChain == testnet (valid, is in offer chains).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					testnetHash,
				),
			)
			require.NoError(t, ValidateInvoiceRequestWrite(ir))

			// InvreqChain == regtest (invalid, not in offer
			// chains).
			ir.InvreqChain = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType80](
					regtestHash,
				),
			)
			require.ErrorIs(
				t, ValidateInvoiceRequestWrite(ir),
				ErrUnsupportedChain,
			)
		})
	})
}

// TestValidateInvoiceRequestReadSentinels table-drives selected reader-side
// validation branches in ValidateInvoiceRequestRead. Each row starts from a
// minimal structurally valid invoice request and mutates one condition to
// assert the corresponding sentinel error, or nil for accepted optional
// unknown fields. Additional reader-side checks such as amount, chain and
// BIP-353 validation are covered by focused tests below.
func TestValidateInvoiceRequestReadSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*InvoiceRequest)
		known   map[lnwire.FeatureBit]string
		wantErr error
	}{
		{
			name: "missing payer id",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.OptionalRecordT[
					tlv.TlvType88, *btcec.PublicKey,
				]{}
			},
			wantErr: ErrMissingPayerID,
		},
		{
			name: "present-but-nil payer_id",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "present-but-nil offer_issuer_id",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "missing metadata",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqMetadata = tlv.OptionalRecordT[
					tlv.TlvType0, tlv.Blob,
				]{}
			},
			wantErr: ErrMissingMetadata,
		},
		{
			name: "missing signature",
			mutate: func(ir *InvoiceRequest) {
				ir.Signature = tlv.OptionalRecordT[
					tlv.TlvType240, [64]byte,
				]{}
			},
			wantErr: ErrMissingSignature,
		},
		{
			name: "missing amount",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqAmount = tlv.OptionalRecordT[
					tlv.TlvType82, TUint64,
				]{}
				ir.OfferAmount = tlv.OptionalRecordT[
					tlv.TlvType8, TUint64,
				]{}
			},
			wantErr: ErrMissingAmount,
		},
		{
			name: "quantity missing with quantity_max",
			mutate: func(ir *InvoiceRequest) {
				_, pub := bobKey()
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						pub,
					),
				)
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(10),
					),
				)
			},
			wantErr: ErrQuantityMissing,
		},
		{
			name: "quantity present but zero with quantity_max",
			mutate: func(ir *InvoiceRequest) {
				_, pub := bobKey()
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						pub,
					),
				)
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(10),
					),
				)
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType86](
						TUint64(0),
					),
				)
			},
			wantErr: ErrQuantityZero,
		},
		{
			name: "quantity exceeds max",
			mutate: func(ir *InvoiceRequest) {
				_, pub := bobKey()
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						pub,
					),
				)
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(5),
					),
				)
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType86](
						TUint64(99),
					),
				)
			},
			wantErr: ErrQuantityExceedsMax,
		},
		{
			name: "invreq_quantity without offer_quantity_max",
			mutate: func(ir *InvoiceRequest) {
				_, pub := bobKey()
				ir.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						pub,
					),
				)
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType86](
						TUint64(1),
					),
				)
			},
			wantErr: ErrQuantityWithoutMax,
		},
		{
			name: "spontaneous request carrying offer field " +
				"(quantity max)",
			mutate: func(ir *InvoiceRequest) {
				ir.OfferQuantityMax = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType20](
						TUint64(10),
					),
				)
			},
			wantErr: ErrOfferFieldsOnSpontaneous,
		},
		{
			name: "spontaneous request carrying quantity",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqQuantity = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType86](
						TUint64(1),
					),
				)
			},
			wantErr: ErrQuantityWithoutMax,
		},
		{
			name: "out-of-range TLV in decoded extras",
			mutate: func(ir *InvoiceRequest) {
				ir.decodedTLVs = tlv.TypeMap{200: nil}
			},
			wantErr: ErrOutOfRangeType,
		},
		{
			name: "unknown even TLV type in range",
			mutate: func(ir *InvoiceRequest) {
				ir.decodedTLVs = tlv.TypeMap{158: nil}
			},
			wantErr: ErrUnknownEvenType,
		},
		{
			name: "unknown even type 34 rejected",
			mutate: func(ir *InvoiceRequest) {
				ir.decodedTLVs = tlv.TypeMap{34: nil}
			},
			wantErr: ErrUnknownEvenType,
		},
		{
			// An unknown odd type in the signature range
			// (240-1000) is a future optional signature element
			// and is ignored, not rejected.
			name: "unknown odd TLV in signature range ignored",
			mutate: func(ir *InvoiceRequest) {
				ir.decodedTLVs = tlv.TypeMap{501: nil}
			},
			wantErr: nil,
		},
		{
			// An unknown even type stays must-understand even
			// inside the signature range.
			name: "unknown even TLV in signature range rejected",
			mutate: func(ir *InvoiceRequest) {
				ir.decodedTLVs = tlv.TypeMap{500: nil}
			},
			wantErr: ErrUnknownEvenType,
		},
		{
			name: "unknown even feature bit",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType84](
						*lnwire.NewRawFeatureVector(0),
					),
				)
			},
			wantErr: ErrUnknownEvenFeature,
		},
		{
			name: "known even feature bit accepted",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType84](
						*lnwire.NewRawFeatureVector(0),
					),
				)
			},
			known: map[lnwire.FeatureBit]string{
				0: "test_feature",
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ir := validInvoiceRequest(t)
			tc.mutate(ir)

			if tc.name == "known even feature bit accepted" {
				ir.Signature = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType240](
						[64]byte{0x01},
					),
				)
			}

			err := ValidateInvoiceRequestRead(
				ir, bitcoinMainnetGenesisHash, tc.known,
			)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestValidateInvoiceRequestReadAmountBelowExpected pins the reader-side
// mirror of the writer's expected-amount rule: when offer_amount is present
// (native bitcoin, no offer_currency) a present invreq_amount below
// offer_amount*invreq_quantity MUST be rejected. The amount check runs before
// the signature verification, so an unsigned struct suffices to exercise it.
func TestValidateInvoiceRequestReadAmountBelowExpected(t *testing.T) {
	t.Parallel()

	_, pub := bobKey()
	ir := &InvoiceRequest{
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](pub),
		),
		OfferAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8](TUint64(1000)),
		),
		OfferQuantityMax: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType20](TUint64(10)),
		),
		InvreqQuantity: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType86](TUint64(2)),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](pub),
		),
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](tlv.Blob("m")),
		),
	}

	// expected = 1000 * 2 = 2000; 1999 is below.
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType82](TUint64(1999)),
	)
	err := ValidateInvoiceRequestRead(ir, bitcoinMainnetGenesisHash, nil)
	require.ErrorIs(t, err, ErrAmountBelowExpected)
}

// TestValidateInvoiceRequestAmountOverflow pins the guard against an
// offer_amount * invreq_quantity product that overflows uint64.
func TestValidateInvoiceRequestAmountOverflow(t *testing.T) {
	t.Parallel()

	_, pub := bobKey()

	newRequest := func() *InvoiceRequest {
		ir := &InvoiceRequest{}
		ir.OfferIssuerID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](pub),
		)
		ir.OfferAmount = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8](TUint64(2)),
		)

		// quantity_max zero means unlimited, so the bound check does
		// not cap the quantity below.
		ir.OfferQuantityMax = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType20](TUint64(0)),
		)
		// offer_amount(2) * quantity(2^63) == 2^64, which truncates to
		// zero on an unchecked uint64 multiply; an unguarded validator
		// would then accept invreq_amount(1) as "at least zero".
		ir.InvreqQuantity = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType86](
				TUint64(1 << 63),
			),
		)
		ir.InvreqAmount = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82](TUint64(1)),
		)
		ir.InvreqPayerID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](pub),
		)
		ir.InvreqMetadata = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](tlv.Blob("m")),
		)

		return ir
	}

	// The reader MUST reject the overflowing request.
	readErr := ValidateInvoiceRequestRead(
		newRequest(), bitcoinMainnetGenesisHash, nil,
	)
	require.ErrorIs(t, readErr, ErrAmountBelowExpected)

	// The writer MUST reject it too (same rule, both sides).
	writeErr := ValidateInvoiceRequestWrite(newRequest())
	require.ErrorIs(t, writeErr, ErrAmountBelowExpected)
}

// TestValidateInvoiceRequestReadChain pins the spec invreq_chain rule:
// an absent invreq_chain defaults to Bitcoin mainnet and must be
// rejected on a non-mainnet node, while a present invreq_chain that
// disagrees with activeChain must also be rejected. The happy path
// (matching chain) is already covered by TestValidateInvoiceRequestRead.
func TestValidateInvoiceRequestReadChain(t *testing.T) {
	t.Parallel()

	var altChain [32]byte
	for i := range altChain {
		altChain[i] = 0xaa
	}

	t.Run("absent chain rejected on non-mainnet", func(t *testing.T) {
		t.Parallel()

		ir := validInvoiceRequest(t)
		err := ValidateInvoiceRequestRead(ir, altChain, nil)
		require.ErrorIs(t, err, ErrUnsupportedChain)
	})

	t.Run("present chain mismatch rejected", func(t *testing.T) {
		t.Parallel()

		ir := validInvoiceRequest(t)
		ir.InvreqChain = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80](altChain),
		)
		// The chain check runs before signature verification, so a
		// mismatched chain is rejected regardless of the signature.
		err := ValidateInvoiceRequestRead(
			ir, bitcoinMainnetGenesisHash, nil,
		)
		require.ErrorIs(t, err, ErrUnsupportedChain)
	})
}

// bip353Blob assembles a name+domain pair into the wire layout expected
// by invreq_bip_353_name (TLV 91).
func bip353Blob(name, domain []byte) []byte {
	out := make([]byte, 0, 2+len(name)+len(domain))
	out = append(out, byte(len(name)))
	out = append(out, name...)
	out = append(out, byte(len(domain)))
	out = append(out, domain...)

	return out
}

// TestCheckBip353Name exercises the BIP 353 alphabet and structural
// requirements directly so each rejection path is pinned independently
// of the surrounding invoice-request validators.
func TestCheckBip353Name(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		blob    []byte
		wantErr bool
	}{
		{
			name: "happy path with allowed alphabet",
			blob: bip353Blob(
				[]byte("alice.example-1_2"),
				[]byte("example.com"),
			),
			wantErr: false,
		},
		{
			name:    "absent field is no-op",
			blob:    nil,
			wantErr: false,
		},
		{
			name:    "present but empty rejected",
			blob:    []byte{},
			wantErr: true,
		},
		{
			name: "empty name rejected",
			blob: []byte{
				0x00, 0x05, 'e', 'x', '.', 'c', 'o', 'm',
			},
			wantErr: true,
		},
		{
			name:    "empty domain rejected",
			blob:    []byte{0x05, 'a', 'l', 'i', 'c', 'e', 0x00},
			wantErr: true,
		},
		{
			name:    "both empty name and domain rejected",
			blob:    []byte{0x00, 0x00},
			wantErr: true,
		},
		{
			name: "name byte outside alphabet",
			blob: bip353Blob(
				[]byte("alice@bob"), []byte("ex.com"),
			),
			wantErr: true,
		},
		{
			name:    "domain byte outside alphabet",
			blob:    bip353Blob([]byte("alice"), []byte("ex com")),
			wantErr: true,
		},
		{
			name:    "name truncated before domain_len",
			blob:    []byte{0x05, 'a', 'l', 'i'},
			wantErr: true,
		},
		{
			name:    "domain length mismatch",
			blob:    []byte{0x01, 'a', 0x05, 'b'},
			wantErr: true,
		},
		{
			name: "control byte rejected in name",
			blob: bip353Blob(
				[]byte{'a', 0x00, 'b'}, []byte("ex"),
			),
			wantErr: true,
		},
		{
			name: "domain length shorter than remaining " +
				"bytes rejected",
			blob:    []byte{0x01, 'a', 0x01, 'b', 'c'},
			wantErr: true,
		},
		{
			name:    "minimal valid name and domain",
			blob:    []byte{0x01, 'a', 0x01, 'b'},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var opt tlv.OptionalRecordT[tlv.TlvType91, tlv.Blob]
			if tc.blob != nil {
				opt = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType91](
						tc.blob,
					),
				)
			}

			err := checkBip353Name(opt)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidBip353Name)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// encodeIRBypassValidate serialises an InvoiceRequest skipping the
// validate-on-encode gate.
func encodeIRBypassValidate(ir *InvoiceRequest) ([]byte, error) {
	records := lnwire.ProduceRecordsSorted(ir.allRecordProducers()...)
	return lnwire.EncodeRecords(records)
}

// encodeInvBypassValidate is the Invoice analogue for encodeIRBypassValidate.
func encodeInvBypassValidate(inv *Invoice) ([]byte, error) {
	records := lnwire.ProduceRecordsSorted(inv.allRecordProducers()...)
	return lnwire.EncodeRecords(records)
}

// TestValidateInvoiceRead table-drives every reader-side rejection in
// ValidateInvoiceRead.
func TestValidateInvoiceRead(t *testing.T) {
	t.Parallel()

	_, intro := aliceKey()
	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	baseline := func() *Invoice {
		return validInvoice(t)
	}

	tests := []struct {
		name    string
		mutate  func(*Invoice)
		wantErr error
	}{
		{
			name: "missing amount",
			mutate: func(inv *Invoice) {
				inv.InvoiceAmount = tlv.OptionalRecordT[
					tlv.TlvType170, TUint64,
				]{}
			},
			wantErr: ErrMissingAmount,
		},
		{
			name: "zero amount",
			mutate: func(inv *Invoice) {
				inv.InvoiceAmount = tlv.SomeRecordT(
					tlv.NewRecordT[
						tlv.TlvType170, TUint64,
					](TUint64(0)),
				)
			},
			wantErr: ErrZeroInvoiceAmount,
		},
		{
			name: "missing created_at",
			mutate: func(inv *Invoice) {
				inv.InvoiceCreatedAt = tlv.OptionalRecordT[
					tlv.TlvType164, TUint64,
				]{}
			},
			wantErr: ErrMissingCreatedAt,
		},
		{
			name: "missing payment_hash",
			mutate: func(inv *Invoice) {
				inv.InvoicePaymentHash = tlv.OptionalRecordT[
					tlv.TlvType168, [32]byte,
				]{}
			},
			wantErr: ErrMissingPaymentHash,
		},
		{
			name: "missing node_id",
			mutate: func(inv *Invoice) {
				inv.InvoiceNodeID = tlv.OptionalRecordT[
					tlv.TlvType176, *btcec.PublicKey,
				]{}
			},
			wantErr: ErrMissingNodeID,
		},
		{
			name: "missing paths",
			mutate: func(inv *Invoice) {
				inv.InvoicePaths = tlv.OptionalRecordT[
					tlv.TlvType160, lnwire.BlindedPaths,
				]{}
			},
			wantErr: ErrMissingPaths,
		},
		{
			name: "missing blinded_pay",
			mutate: func(inv *Invoice) {
				inv.InvoiceBlindedPay = tlv.OptionalRecordT[
					tlv.TlvType162, BlindedPayInfos,
				]{}
			},
			wantErr: ErrMissingBlindedPay,
		},
		{
			name: "paths count exceeds blinded_pay count",
			mutate: func(inv *Invoice) {
				path := lnwire.BlindedPath{
					IntroductionNode: introNode,
					Hops: []lnwire.BlindedHop{
						{},
					},
				}
				paths := lnwire.BlindedPaths{
					Paths: []lnwire.BlindedPath{path, path},
				}
				inv.InvoicePaths = tlv.SomeRecordT(
					tlv.NewRecordT[
						tlv.TlvType160,
						lnwire.BlindedPaths,
					](paths),
				)
			},
			wantErr: ErrBlindedPayMismatch,
		},
		{
			// The invoice reader defines no out-of-range type
			// rejection, so an unknown odd type outside every known
			// range is ignored rather than rejected: validation
			// proceeds to the final signature check.
			name: "unknown odd out-of-range type ignored",
			mutate: func(inv *Invoice) {
				inv.decodedTLVs = tlv.TypeMap{2001: nil}
			},
			wantErr: ErrMissingSignature,
		},
		{
			name: "unknown even type",
			mutate: func(inv *Invoice) {
				inv.decodedTLVs = tlv.TypeMap{200: nil}
			},
			wantErr: ErrUnknownEvenType,
		},
		{
			// Baseline invoice_node_id is bob; an offer_issuer_id
			// of alice must be rejected as a mismatch.
			name: "node_id does not match offer_issuer_id",
			mutate: func(inv *Invoice) {
				_, alice := aliceKey()
				inv.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						alice,
					),
				)
			},
			wantErr: ErrInvoiceNodeIDMismatch,
		},
		{
			// A present-but-nil invoice_node_id passes IsSome but
			// would panic the codec on encode, so it must be
			// rejected as ErrNilPublicKey rather than treated as a
			// missing or mismatched field.
			name: "present-but-nil node_id",
			mutate: func(inv *Invoice) {
				inv.InvoiceNodeID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType176](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr: ErrNilPublicKey,
		},
		{
			name: "unsupported chain",
			mutate: func(inv *Invoice) {
				inv.InvreqChain = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType80](
						[32]byte{0x01},
					),
				)
			},
			wantErr: ErrUnsupportedChain,
		},
		{
			name: "unknown even feature",
			mutate: func(inv *Invoice) {
				fv := *lnwire.NewRawFeatureVector(0)
				inv.InvoiceFeatures = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType174](fv),
				)
			},
			wantErr: ErrUnknownEvenFeature,
		},
		{
			name: "empty invoice_paths",
			mutate: func(inv *Invoice) {
				paths := lnwire.BlindedPaths{Paths: nil}
				inv.InvoicePaths = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType160](paths),
				)
			},
			wantErr: ErrEmptyBlindedPaths,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := baseline()
			tc.mutate(inv)

			err := ValidateInvoiceRead(
				inv, bitcoinMainnetGenesisHash,
				InvoiceFeatureCatalogues{},
			)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestValidateInvoiceReadAcceptsSignatureRange pins the rule that an unknown
// odd TLV anywhere in the signature range (240-1000) is ignored rather than
// rejected.
func TestValidateInvoiceReadAcceptsSignatureRange(t *testing.T) {
	t.Parallel()

	_, pub := bobKey()

	_, intro := aliceKey()
	_, blinding := bobKey()
	_, hopPub := aliceKey()
	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	inv := &Invoice{
		InvoiceCreatedAt: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType164, TUint64](TUint64(123)),
		),
		InvoiceAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType170, TUint64](TUint64(1000)),
		),
		InvoicePaymentHash: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType168, [32]byte](
				[32]byte{},
			),
		),
		InvoiceNodeID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType176](pub),
		),
		InvoicePaths: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160, lnwire.BlindedPaths](
				lnwire.BlindedPaths{
					Paths: []lnwire.BlindedPath{{
						IntroductionNode: introNode,
						BlindingPoint:    blinding,
						Hops: []lnwire.BlindedHop{{
							BlindedNodeID: hopPub,
						}},
					}},
				},
			),
		),
		InvoiceBlindedPay: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162, BlindedPayInfos](
				BlindedPayInfos{Infos: []BlindedPayInfo{{}}},
			),
		),
		Signature: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](
				[64]byte{},
			),
		),
	}

	// An unknown odd type at 241 sits inside the signature range and must
	// be ignored, not rejected as out-of-range or unknown-even.
	inv.decodedTLVs = tlv.TypeMap{241: nil}

	err = ValidateInvoiceRead(
		inv, bitcoinMainnetGenesisHash,
		InvoiceFeatureCatalogues{},
	)
	require.NoError(t, err)
}

// TestValidateInvoiceExpiry covers the relative-expiry default, an explicit
// relative expiry, the expired/not-expired boundary, and the overflow guard
// that keeps an absurd created_at from wrapping into a spurious expiry.
func TestValidateInvoiceExpiry(t *testing.T) {
	t.Parallel()

	invoice := func(createdAt uint64, relExp *uint32) *Invoice {
		inv := &Invoice{
			InvoiceCreatedAt: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType164, TUint64](
					TUint64(createdAt),
				),
			),
		}
		if relExp != nil {
			inv.InvoiceRelativeExp = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType166, TUint32](
					TUint32(*relExp),
				),
			)
		}

		return inv
	}

	relExp := func(v uint32) *uint32 { return &v }

	tests := []struct {
		name    string
		inv     *Invoice
		now     int64
		wantErr error
	}{
		{
			name:    "missing created_at",
			inv:     &Invoice{},
			now:     1000,
			wantErr: ErrMissingCreatedAt,
		},
		{
			name: "within default expiry",
			inv:  invoice(1000, nil),
			now:  1000 + 7199,
		},
		{
			// The boundary second itself is still valid: the spec
			// rejects only when now is strictly greater than
			// created_at + expiry.
			name: "at default expiry boundary",
			inv:  invoice(1000, nil),
			now:  1000 + 7200,
		},
		{
			name:    "past default expiry",
			inv:     invoice(1000, nil),
			now:     1000 + 7201,
			wantErr: ErrInvoiceExpired,
		},
		{
			name: "within explicit expiry",
			inv:  invoice(1000, relExp(100)),
			now:  1099,
		},
		{
			// The exact expiry second is still valid (strict ">").
			name: "at explicit expiry boundary",
			inv:  invoice(1000, relExp(100)),
			now:  1100,
		},
		{
			name:    "past explicit expiry",
			inv:     invoice(1000, relExp(100)),
			now:     1101,
			wantErr: ErrInvoiceExpired,
		},
		{
			name: "overflow is not expired",
			inv:  invoice(math.MaxUint64, relExp(100)),
			now:  9223372036854775807,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateInvoiceExpiry(
				tc.inv, time.Unix(tc.now, 0),
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
		})
	}
}

// TestValidateInvoiceAgainstRequest table-drives the mirror-field comparison
// between an invoice and the request it is responding to.
func TestValidateInvoiceAgainstRequest(t *testing.T) {
	t.Parallel()

	// The request whose mirrored fields every invoice below is compared
	// against: payer metadata plus offer_amount.
	ir := &InvoiceRequest{
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				[]byte("metadata"),
			),
		),
		OfferAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8, TUint64](1000),
		),
	}

	irEncoded, err := encodeIRBypassValidate(ir)
	require.NoError(t, err)
	irDecoded, err := DecodeInvoiceRequest(irEncoded)
	require.NoError(t, err)

	// baseline mirrors the request's fields exactly. Invoice-specific
	// fields >= 160 (here invoice_amount) are excluded from the mirror
	// comparison, so the baseline validates cleanly.
	baseline := func() *Invoice {
		return &Invoice{
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
			OfferAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType8, TUint64](1000),
			),
			InvoiceAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType170, TUint64](1000),
			),
		}
	}

	tests := []struct {
		name        string
		mutate      func(*Invoice)
		wantErr     error
		errContains string
	}{
		{
			name:   "matching mirrored fields",
			mutate: func(inv *Invoice) {},
		},
		{
			name: "missing mirrored field",
			mutate: func(inv *Invoice) {
				inv.OfferAmount = tlv.OptionalRecordT[
					tlv.TlvType8, TUint64,
				]{}
			},
			wantErr:     ErrInvoiceMismatch,
			errContains: "missing 1 fields",
		},
		{
			name: "extra mirrored field",
			mutate: func(inv *Invoice) {
				inv.OfferDescription = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType10](
						[]byte("extra"),
					),
				)
			},
			wantErr:     ErrInvoiceMismatch,
			errContains: "unexpected field 10",
		},
		{
			name: "mismatched field data",
			mutate: func(inv *Invoice) {
				inv.InvreqMetadata = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType0](
						[]byte("different"),
					),
				)
			},
			wantErr:     ErrInvoiceMismatch,
			errContains: "data mismatch",
		},
		{
			name: "equal length byte difference",
			mutate: func(inv *Invoice) {
				inv.InvreqMetadata = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType0](
						[]byte("metadatA"),
					),
				)
			},
			wantErr:     ErrInvoiceMismatch,
			errContains: "data mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := baseline()
			tc.mutate(inv)

			invEncoded, err := encodeInvBypassValidate(inv)
			require.NoError(t, err)
			invDecoded, err := DecodeInvoice(invEncoded)
			require.NoError(t, err)

			err = ValidateInvoiceAgainstRequest(
				invDecoded, irDecoded,
			)
			if tc.wantErr == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// TestValidateInvoiceAgainstRequestAmountMirror covers the cross-field
// invreq_amount (82) vs invoice_amount (170) equality rule.
func TestValidateInvoiceAgainstRequestAmountMirror(t *testing.T) {
	t.Parallel()

	ir := &InvoiceRequest{
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				[]byte("metadata"),
			),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](2500),
		),
	}
	irEncoded, err := encodeIRBypassValidate(ir)
	require.NoError(t, err)
	irDecoded, err := DecodeInvoiceRequest(irEncoded)
	require.NoError(t, err)

	build := func(invAmt uint64) *Invoice {
		return &Invoice{
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
			InvreqAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType82, TUint64](2500),
			),
			InvoiceAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType170, TUint64](
					TUint64(invAmt),
				),
			),
		}
	}

	// Equal amounts pass.
	matchEnc, _ := encodeInvBypassValidate(build(2500))
	matchDec, _ := DecodeInvoice(matchEnc)
	require.NoError(t, ValidateInvoiceAgainstRequest(matchDec, irDecoded))

	// Mismatched amounts fail.
	missEnc, _ := encodeInvBypassValidate(build(2501))
	missDec, _ := DecodeInvoice(missEnc)
	err = ValidateInvoiceAgainstRequest(missDec, irDecoded)
	require.ErrorIs(t, err, ErrInvoiceMismatch)
	require.Contains(t, err.Error(), "invoice_amount")
}

// TestValidateInvoiceAgainstRequestOfferAmount pins the offer-amount lower
// bound applied when invreq_amount is absent: the payee MUST NOT charge less
// than offer_amount * invreq_quantity for the native (non-offer_currency) case,
// while the offer_currency case is delegated to the caller.
func TestValidateInvoiceAgainstRequestOfferAmount(t *testing.T) {
	t.Parallel()

	// build constructs a mirrored (invoice, request) pair carrying a fixed
	// offer_amount and optional quantity/currency, with no invreq_amount so
	// the offer-amount bound is what gets exercised.
	build := func(offerAmt uint64, qty *uint64, currency []byte,
		invAmt uint64) (*Invoice, *InvoiceRequest) {

		ir := &InvoiceRequest{
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
			OfferAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType8, TUint64](
					TUint64(offerAmt),
				),
			),
		}
		inv := &Invoice{
			InvreqMetadata: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType0](
					[]byte("metadata"),
				),
			),
			OfferAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType8, TUint64](
					TUint64(offerAmt),
				),
			),
			InvoiceAmount: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType170, TUint64](
					TUint64(invAmt),
				),
			),
		}
		if qty != nil {
			ir.InvreqQuantity = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType86, TUint64](
					TUint64(*qty),
				),
			)
			inv.InvreqQuantity = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType86, TUint64](
					TUint64(*qty),
				),
			)
		}
		if currency != nil {
			ir.OfferCurrency = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType6](currency),
			)
			inv.OfferCurrency = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType6](currency),
			)
		}

		return inv, ir
	}

	// roundtrip encodes and decodes both sides so the comparison runs over
	// canonical wire bytes, mirroring the production flow.
	roundtrip := func(inv *Invoice, ir *InvoiceRequest) error {
		irEnc, err := encodeIRBypassValidate(ir)
		require.NoError(t, err)
		irDec, err := DecodeInvoiceRequest(irEnc)
		require.NoError(t, err)

		invEnc, err := encodeInvBypassValidate(inv)
		require.NoError(t, err)
		invDec, err := DecodeInvoice(invEnc)
		require.NoError(t, err)

		return ValidateInvoiceAgainstRequest(invDec, irDec)
	}

	qty := func(v uint64) *uint64 { return &v }

	t.Run("at offer amount passes", func(t *testing.T) {
		t.Parallel()

		inv, ir := build(1000, nil, nil, 1000)
		require.NoError(t, roundtrip(inv, ir))
	})

	t.Run("above offer amount passes", func(t *testing.T) {
		t.Parallel()

		inv, ir := build(1000, nil, nil, 2000)
		require.NoError(t, roundtrip(inv, ir))
	})

	t.Run("below offer amount rejected", func(t *testing.T) {
		t.Parallel()

		inv, ir := build(1000, nil, nil, 999)
		require.ErrorIs(t, roundtrip(inv, ir), ErrAmountBelowExpected)
	})

	t.Run("quantity scales the bound", func(t *testing.T) {
		t.Parallel()

		// 1000 * 3 = 3000 expected; 2999 is below, 3000 at the bound.
		inv, ir := build(1000, qty(3), nil, 2999)
		require.ErrorIs(t, roundtrip(inv, ir), ErrAmountBelowExpected)

		inv, ir = build(1000, qty(3), nil, 3000)
		require.NoError(t, roundtrip(inv, ir))
	})

	t.Run("offer_currency bound delegated", func(t *testing.T) {
		t.Parallel()

		// With offer_currency present the bitcoin-unit bound does not
		// apply, so an invoice_amount below offer_amount still passes
		// this validator; the caller applies the exchange-rate check.
		inv, ir := build(1000, nil, []byte("USD"), 1)
		require.NoError(t, roundtrip(inv, ir))
	})
}

// TestValidateInvoiceWrite table-drives the writer-side checks of
// ValidateInvoiceWrite by clearing required fields on a valid baseline invoice.
func TestValidateInvoiceWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Invoice)
		wantErr error
	}{
		{
			name:    "valid baseline invoice",
			mutate:  func(inv *Invoice) {},
			wantErr: nil,
		},
		{
			name: "missing created_at",
			mutate: func(inv *Invoice) {
				inv.InvoiceCreatedAt = tlv.OptionalRecordT[
					tlv.TlvType164, TUint64,
				]{}
			},
			wantErr: ErrMissingCreatedAt,
		},
		{
			name: "missing amount",
			mutate: func(inv *Invoice) {
				inv.InvoiceAmount = tlv.OptionalRecordT[
					tlv.TlvType170, TUint64,
				]{}
			},
			wantErr: ErrMissingAmount,
		},
		{
			name: "missing payment_hash",
			mutate: func(inv *Invoice) {
				inv.InvoicePaymentHash = tlv.OptionalRecordT[
					tlv.TlvType168, [32]byte,
				]{}
			},
			wantErr: ErrMissingPaymentHash,
		},
		{
			name: "missing node_id",
			mutate: func(inv *Invoice) {
				inv.InvoiceNodeID = tlv.OptionalRecordT[
					tlv.TlvType176, *btcec.PublicKey,
				]{}
			},
			wantErr: ErrMissingNodeID,
		},
		{
			name: "missing paths",
			mutate: func(inv *Invoice) {
				inv.InvoicePaths = tlv.OptionalRecordT[
					tlv.TlvType160, lnwire.BlindedPaths,
				]{}
			},
			wantErr: ErrMissingPaths,
		},
		{
			name: "missing blinded_pay",
			mutate: func(inv *Invoice) {
				inv.InvoiceBlindedPay = tlv.OptionalRecordT[
					tlv.TlvType162, BlindedPayInfos,
				]{}
			},
			wantErr: ErrMissingBlindedPay,
		},
		{
			name: "present non-nil payer_id and matching " +
				"non-nil offer_issuer_id",
			mutate: func(inv *Invoice) {
				_, payerID := bobKey()
				_, issuerID := aliceKey()
				inv.InvreqPayerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						payerID,
					),
				)
				inv.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						issuerID,
					),
				)
				inv.InvoiceNodeID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType176](
						issuerID,
					),
				)
			},
			wantErr: nil,
		},
		{
			name: "mismatched node_id and offer_issuer_id",
			mutate: func(inv *Invoice) {
				_, issuerID := aliceKey()
				inv.OfferIssuerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType22](
						issuerID,
					),
				)
			},
			wantErr: ErrInvoiceNodeIDMismatch,
		},
		{
			name: "zero invoice_amount",
			mutate: func(inv *Invoice) {
				inv.InvoiceAmount = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType170](
						TUint64(0),
					),
				)
			},
			wantErr: ErrZeroInvoiceAmount,
		},
		{
			name: "blinded pay info mismatch",
			mutate: func(inv *Invoice) {
				infos := BlindedPayInfos{
					Infos: []BlindedPayInfo{{}, {}},
				}
				inv.InvoiceBlindedPay = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType162](infos),
				)
			},
			wantErr: ErrBlindedPayMismatch,
		},
		{
			name: "empty invoice_paths",
			mutate: func(inv *Invoice) {
				paths := lnwire.BlindedPaths{Paths: nil}
				inv.InvoicePaths = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType160](paths),
				)
			},
			wantErr: ErrEmptyBlindedPaths,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := validInvoice(t)
			tc.mutate(inv)

			err := ValidateInvoiceWrite(inv)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

// TestValidateFeaturesWithCatalogue verifies that both Role 1 endpoint features
// and Role 2 routing path features are correctly validated using injected
// catalogues.
func TestValidateFeaturesWithCatalogue(t *testing.T) {
	t.Parallel()

	// Role 1 validation verifies endpoint features on ValidateInvoiceRead.
	t.Run("endpoint features (Role 1)", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		inv.Signature = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240](
				[64]byte{},
			),
		)

		// Set MPP required (bit 16, even/required)
		fv := *lnwire.NewRawFeatureVector(lnwire.MPPRequired)
		inv.InvoiceFeatures = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType174](fv),
		)

		// An unknown required bit must be rejected.
		err := ValidateInvoiceRead(
			inv, bitcoinMainnetGenesisHash,
			InvoiceFeatureCatalogues{},
		)
		require.ErrorIs(t, err, ErrUnknownEvenFeature)

		// A known required bit must pass.
		known := map[lnwire.FeatureBit]string{
			lnwire.MPPRequired: "mpp",
		}
		err = ValidateInvoiceRead(
			inv, bitcoinMainnetGenesisHash,
			InvoiceFeatureCatalogues{Invoice: known},
		)
		require.NoError(t, err)
	})

	// Role 2 validation verifies routing path features on
	// ValidateInvoiceRead.
	t.Run("routing path features (Role 2)", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		inv.Signature = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240](
				[64]byte{},
			),
		)

		// Set an even required feature bit on the path's features (e.g.
		// bit 16).
		fv := *lnwire.NewRawFeatureVector(lnwire.MPPRequired)
		inv.InvoiceBlindedPay = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162](BlindedPayInfos{
				Infos: []BlindedPayInfo{{
					Features: fv,
				}},
			}),
		)

		// If there are no known features in the catalogue, there are
		// zero usable paths and we expect ErrNoUsablePaths.
		err := ValidateInvoiceRead(
			inv, bitcoinMainnetGenesisHash,
			InvoiceFeatureCatalogues{},
		)
		require.ErrorIs(t, err, ErrNoUsablePaths)

		// A known features catalogue for blinded pay results in at
		// least one usable path, which must pass.
		knownBlinded := map[lnwire.FeatureBit]string{
			lnwire.MPPRequired: "mpp",
		}
		err = ValidateInvoiceRead(
			inv, bitcoinMainnetGenesisHash,
			InvoiceFeatureCatalogues{Blinded: knownBlinded},
		)
		require.NoError(t, err)
	})

	// Writer side ignores features, as we set the features.
	t.Run("writer side ignores features", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		fv := *lnwire.NewRawFeatureVector(lnwire.MPPRequired)
		inv.InvoiceFeatures = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType174](fv),
		)

		require.NoError(t, ValidateInvoiceWrite(inv))
	})
}

// TestValidateInvoiceWriteRejectsNilPubkeys verifies the writer rejects a
// present-but-nil mirrored pubkey field, which would otherwise panic the codec
// on encode. Symmetric with ValidateInvoiceRequestWrite.
func TestValidateInvoiceWriteRejectsNilPubkeys(t *testing.T) {
	t.Parallel()

	t.Run("present-but-nil payer_id", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		inv.InvreqPayerID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](
				(*btcec.PublicKey)(nil),
			),
		)
		require.ErrorIs(t, ValidateInvoiceWrite(inv), ErrNilPublicKey)
	})

	t.Run("present-but-nil offer_issuer_id", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		inv.OfferIssuerID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](
				(*btcec.PublicKey)(nil),
			),
		)
		require.ErrorIs(t, ValidateInvoiceWrite(inv), ErrNilPublicKey)
	})

	t.Run("present-but-nil node_id", func(t *testing.T) {
		t.Parallel()

		inv := validInvoice(t)
		inv.InvoiceNodeID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType176](
				(*btcec.PublicKey)(nil),
			),
		)
		require.ErrorIs(t, ValidateInvoiceWrite(inv), ErrNilPublicKey)
	})
}

// TestValidateInvoiceErrorWrite verifies the BOLT 12 invoice_error writer
// requirements: error is mandatory and must be a non-empty UTF-8 string, and
// suggested_value may only accompany a set erroneous_field.
func TestValidateInvoiceErrorWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ie      *InvoiceError
		wantErr error
	}{
		{
			name:    "missing error",
			ie:      &InvoiceError{},
			wantErr: ErrMissingError,
		},
		{
			name:    "empty error",
			ie:      &InvoiceError{Error: someError("")},
			wantErr: ErrEmptyError,
		},
		{
			name: "non-utf8 error",
			ie: &InvoiceError{
				Error: someError(string([]byte{0xff, 0xfe})),
			},
			wantErr: ErrInvalidUTF8,
		},
		{
			name: "suggested without erroneous field",
			ie: &InvoiceError{
				SuggestedValue: someSuggested([]byte{0x01}),
				Error:          someError("bad"),
			},
			wantErr: ErrSuggestedWithoutField,
		},
		{
			// The spec permits erroneous_field on its own;
			// suggested_value is only "MAY set" once
			// erroneous_field is present.
			name: "erroneous field without suggested value",
			ie: &InvoiceError{
				ErroneousField: someErrField(82),
				Error:          someError("bad amount"),
			},
			wantErr: nil,
		},
		{
			name: "valid with all fields",
			ie: &InvoiceError{
				ErroneousField: someErrField(82),
				SuggestedValue: someSuggested([]byte{0x01}),
				Error:          someError("bad amount"),
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateInvoiceErrorWrite(tc.ie)
			if tc.wantErr == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}
