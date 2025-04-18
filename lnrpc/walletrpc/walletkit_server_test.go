//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/stretchr/testify/require"
)

// TestWitnessTypeMapping tests that the two witness type enums in the `input`
// package and the `walletrpc` package remain equal.
func TestWitnessTypeMapping(t *testing.T) {
	t.Parallel()

	// Tests that both enum types have the same length except the
	// UNKNOWN_WITNESS type which is only present in the walletrpc
	// witness type enum.
	require.Equal(
		t, len(allWitnessTypes), len(WitnessType_name)-1,
		"number of witness types should match proto definition",
	)

	// Tests that the string representations of both enum types are
	// equivalent.
	for witnessType, witnessTypeProto := range allWitnessTypes {
		// Redeclare to avoid loop variables being captured
		// by func literal.
		witnessType := witnessType
		witnessTypeProto := witnessTypeProto

		t.Run(witnessType.String(), func(tt *testing.T) {
			tt.Parallel()

			witnessTypeName := witnessType.String()
			witnessTypeName = strings.ToUpper(witnessTypeName)
			witnessTypeProtoName := witnessTypeProto.String()
			witnessTypeProtoName = strings.ReplaceAll(
				witnessTypeProtoName, "_", "",
			)

			require.Equal(
				t, witnessTypeName, witnessTypeProtoName,
				"mapped witness types should be named the same",
			)
		})
	}
}

type mockCoinSelectionLocker struct {
	fail bool
}

func (m *mockCoinSelectionLocker) WithCoinSelectLock(f func() error) error {
	if err := f(); err != nil {
		return err
	}

	if m.fail {
		return fmt.Errorf("kek")
	}

	return nil
}

// TestFundPsbtCoinSelect tests that the coin selection for a PSBT template
// works as expected.
func TestFundPsbtCoinSelect(t *testing.T) {
	t.Parallel()

	const fundAmt = 50_000
	var (
		p2wkhDustLimit = lnwallet.DustLimitForSize(input.P2WPKHSize)
		p2trDustLimit  = lnwallet.DustLimitForSize(input.P2TRSize)
		p2wkhScript, _ = input.WitnessPubKeyHash([]byte{})
		p2trScript, _  = txscript.PayToTaprootScript(
			&input.TaprootNUMSKey,
		)
	)

	makePacket := func(outs ...*wire.TxOut) *psbt.Packet {
		p := &psbt.Packet{
			UnsignedTx: &wire.MsgTx{},
		}

		for _, out := range outs {
			p.UnsignedTx.TxOut = append(p.UnsignedTx.TxOut, out)
			p.Outputs = append(p.Outputs, psbt.POutput{})
		}

		return p
	}
	updatePacket := func(p *psbt.Packet,
		f func(*psbt.Packet) *psbt.Packet) *psbt.Packet {

		return f(p)
	}
	calcFee := func(p2trIn, p2wkhIn, p2trOut, p2wkhOut int,
		dust btcutil.Amount) btcutil.Amount {

		estimator := input.TxWeightEstimator{}
		for i := 0; i < p2trIn; i++ {
			estimator.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
		for i := 0; i < p2wkhIn; i++ {
			estimator.AddP2WKHInput()
		}
		for i := 0; i < p2trOut; i++ {
			estimator.AddP2TROutput()
		}
		for i := 0; i < p2wkhOut; i++ {
			estimator.AddP2WKHOutput()
		}

		weight := estimator.Weight()
		fee := chainfee.FeePerKwFloor.FeeForWeight(weight)

		return fee + dust
	}

	testCases := []struct {
		name        string
		utxos       []*lnwallet.Utxo
		packet      *psbt.Packet
		changeIndex int32
		changeType  chanfunding.ChangeAddressType
		feeRate     chainfee.SatPerKWeight

		// expectedUtxoIndexes is the list of utxo indexes that are
		// expected to be used for funding the psbt.
		expectedUtxoIndexes []int

		// expectChangeOutputIndex is the expected output index that is
		// returned from the tested method.
		expectChangeOutputIndex int32

		// expectedChangeOutputAmount is the expected final total amount
		// of the output marked as the change output. This will only be
		// checked if the expected amount is non-zero.
		expectedChangeOutputAmount btcutil.Amount

		// expectedFee is the total amount of fees paid by the funded
		// packet in bytes.
		expectedFee btcutil.Amount

		// maxFeeRatio is the maximum fee to total output amount ratio
		// that we consider valid.
		maxFeeRatio float64

		// expectedErr is the expected concrete error. If not nil, then
		// the error must match exactly.
		expectedErr error

		// expectedContainedErrStr is the expected string to be
		// contained in the returned error.
		expectedContainedErrStr string

		// expectedErrType is the expected error type. If not nil, then
		// the error must be of this type.
		expectedErrType error
	}{{
		name:  "no utxos",
		utxos: []*lnwallet.Utxo{},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:     -1,
		feeRate:         chainfee.FeePerKwFloor,
		maxFeeRatio:     chanfunding.DefaultMaxFeeRatio,
		expectedErrType: &chanfunding.ErrInsufficientFunds{},
	}, {
		name: "1 p2wpkh utxo, add p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 1, 1, 0),
	}, {
		name: "1 p2wpkh utxo, add p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 2, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, exact amount",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 123,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, p2wpkh change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2wkhDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2wkhDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, no change, p2tr change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2trDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2trDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, existing p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, dust change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + calcFee(0, 1, 0, 1, 0) + 50,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, existing " +
			"p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(2, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, add p2tr " +
			"change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(2, 1, 2, 0, 0),
	}, {
		name: "large existing p2tr input, fee estimation p2wpkh " +
			"change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 3,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: 1,
		expectedChangeOutputAmount: fundAmt*3 - fundAmt -
			calcFee(1, 0, 1, 1, 0),
		expectedFee: calcFee(1, 0, 1, 1, 0),
	}, {
		name:  "large existing p2tr input, fee estimation no change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value: fundAmt +
						int64(calcFee(1, 0, 1, 0, 0)),
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(1, 0, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, invalid fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "fee 0.00000111 BTC exceeds max fee " +
			"(0.00000027 BTC) on total output value",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, negative feeratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio * (-1),
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "maxFeeRatio must be between 0.00 " +
			"and 1.00 got -0.20",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, big fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             0.85,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "large existing p2tr input, fee estimation existing " +
			"change output",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 2,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:                0,
		feeRate:                    chainfee.FeePerKwFloor,
		maxFeeRatio:                chanfunding.DefaultMaxFeeRatio,
		changeType:                 chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:        []int{},
		expectChangeOutputIndex:    0,
		expectedChangeOutputAmount: fundAmt*2 - calcFee(1, 0, 1, 0, 0),
		expectedFee:                calcFee(1, 0, 1, 0, 0),
	}}

	for _, tc := range testCases {
		tc := tc

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		walletMock := &mock.WalletController{
			RootKey: privKey,
			Utxos:   tc.utxos,
		}
		rpcServer, _, err := New(&Config{
			Wallet:                walletMock,
			CoinSelectionLocker:   &mockCoinSelectionLocker{},
			CoinSelectionStrategy: wallet.CoinSelectionLargest,
		})
		require.NoError(t, err)

		t.Run(tc.name, func(tt *testing.T) {
			// To avoid our packet being mutated, we'll make a deep
			// copy of it, so we can still use the original in the
			// test case to compare the results to.
			var buf bytes.Buffer
			err := tc.packet.Serialize(&buf)
			require.NoError(tt, err)

			copiedPacket, err := psbt.NewFromRawBytes(&buf, false)
			require.NoError(tt, err)

			resp, err := rpcServer.fundPsbtCoinSelect(
				"", tc.changeIndex, copiedPacket, 0,
				tc.changeType, tc.feeRate,
				rpcServer.cfg.CoinSelectionStrategy,
				tc.maxFeeRatio, nil, 0,
			)

			switch {
			case tc.expectedErr != nil:
				require.Error(tt, err)
				require.ErrorIs(tt, err, tc.expectedErr)

				return

			case tc.expectedErrType != nil:
				require.Error(tt, err)
				require.ErrorAs(tt, err, &tc.expectedErr)

				return
			case tc.expectedContainedErrStr != "":
				require.ErrorContains(
					tt, err, tc.expectedContainedErrStr,
				)

				return
			}

			require.NoError(tt, err)
			require.NotNil(tt, resp)

			resultPacket, err := psbt.NewFromRawBytes(
				bytes.NewReader(resp.FundedPsbt), false,
			)
			require.NoError(tt, err)
			resultTx := resultPacket.UnsignedTx

			expectedNumInputs := len(tc.expectedUtxoIndexes) +
				len(tc.packet.Inputs)
			require.Len(tt, resultPacket.Inputs, expectedNumInputs)
			require.Len(tt, resultTx.TxIn, expectedNumInputs)
			require.Equal(
				tt, tc.expectChangeOutputIndex,
				resp.ChangeOutputIndex,
			)

			fee, err := resultPacket.GetTxFee()
			require.NoError(tt, err)
			require.EqualValues(tt, tc.expectedFee, fee)

			if tc.expectedChangeOutputAmount != 0 {
				changeIdx := resp.ChangeOutputIndex
				require.GreaterOrEqual(tt, changeIdx, int32(-1))
				require.Less(
					tt, changeIdx,
					int32(len(resultTx.TxOut)),
				)

				changeOut := resultTx.TxOut[changeIdx]

				require.EqualValues(
					tt, tc.expectedChangeOutputAmount,
					changeOut.Value,
				)
			}
		})
	}
}
