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
	}

	for _, tc := range tests {
		t.Run(
			tc.name,
			func(t *testing.T) {
				t.Parallel()

				o := validBobOffer(t)
				tc.mutate(o)

				err := ValidateOfferRead(o, now, tc.activeChain)
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

// TestValidateInvoiceRequestReadSentinels table-drives every
// reader-side rejection in ValidateInvoiceRequestRead. The original
// TestValidateInvoiceRequestRead pinned only the happy path and the
// missing-signature branch; the rest of the sentinels in
// validate.go's invoice request range were unverified. Each row
// strips one required field from a known-good signed request and
// asserts the corresponding sentinel fires.
func TestValidateInvoiceRequestReadSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*InvoiceRequest)
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
			name: "missing metadata",
			mutate: func(ir *InvoiceRequest) {
				ir.InvreqMetadata = tlv.OptionalRecordT[
					tlv.TlvType0, tlv.Blob,
				]{}
			},
			wantErr: ErrMissingMetadata,
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
			wantErr: ErrOutOfRangeType,
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ir := validInvoiceRequest(t)
			tc.mutate(ir)

			err := ValidateInvoiceRequestRead(
				ir, bitcoinMainnetGenesisHash,
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
	err := ValidateInvoiceRequestRead(ir, bitcoinMainnetGenesisHash)
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
		newRequest(), bitcoinMainnetGenesisHash,
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
		err := ValidateInvoiceRequestRead(ir, altChain)
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
			ir, bitcoinMainnetGenesisHash,
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
