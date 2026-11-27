package contractcourt

import (
	crand "crypto/rand"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// auxBreachTestKeys holds the keys the breached aux HTLC outputs of a test
// are derived from.
type auxBreachTestKeys struct {
	revokeBasePub *btcec.PublicKey
	commitSecret  *btcec.PrivateKey
	revokeKey     *btcec.PublicKey
}

// newAuxBreachTestKeys generates a random set of breach test keys.
func newAuxBreachTestKeys(t *testing.T) *auxBreachTestKeys {
	t.Helper()

	revokeBasePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	commitSecret, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &auxBreachTestKeys{
		revokeBasePub: revokeBasePriv.PubKey(),
		commitSecret:  commitSecret,
		revokeKey: input.DeriveRevocationPubkey(
			revokeBasePriv.PubKey(), commitSecret.PubKey(),
		),
	}
}

// secondLevelPkScript returns the pkScript of the second-level output that
// is expected for the given second-level tap tweak.
func (k *auxBreachTestKeys) secondLevelPkScript(t *testing.T,
	tapTweak [32]byte) []byte {

	t.Helper()

	outputKey := txscript.ComputeTaprootOutputKey(
		k.revokeKey, tapTweak[:],
	)
	pkScript, err := input.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return pkScript
}

// randomTapTweak returns a random second-level tap tweak.
func randomTapTweak(t *testing.T) [32]byte {
	t.Helper()

	var tweak [32]byte
	_, err := crand.Read(tweak[:])
	require.NoError(t, err)

	return tweak
}

// randomPkScript returns the pkScript of a random taproot key.
func randomPkScript(t *testing.T) []byte {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript, err := input.PayToTaprootScript(key.PubKey())
	require.NoError(t, err)

	return pkScript
}

// htlcOutput returns a breached first-level HTLC output of the given type
// at the given commitment output index. With aux set the output carries a
// resolution request template and a first-level blob, like the HTLC of an
// aux channel does.
func (k *auxBreachTestKeys) htlcOutput(t *testing.T, index uint32,
	witnessType input.StandardWitnessType, tapTweak [32]byte,
	aux bool) breachedOutput {

	t.Helper()

	bo := breachedOutput{
		amt:         1000,
		outpoint:    wire.OutPoint{Index: index},
		witnessType: witnessType,
		signDesc: input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: k.revokeBasePub,
			},
			DoubleTweak: k.commitSecret,
			Output: &wire.TxOut{
				Value:    1000,
				PkScript: randomPkScript(t),
			},
		},
		secondLevelTapTweak: tapTweak,
	}

	if aux {
		incoming := witnessType == input.TaprootHtlcAcceptedRevoke
		bo.resolveReq = &lnwallet.ResolutionReq{
			Type:         witnessType,
			HtlcID:       fn.Some(uint64(index)),
			HtlcIncoming: fn.Some(incoming),
			KeyRing:      &lnwallet.CommitmentKeyRing{},
		}
		bo.resolutionBlob = fn.Some(tlv.Blob{0xf1, byte(index)})
	}

	return bo
}

// secondLevelSpend returns the spend of the breached output at the given
// index by a transaction with the given outputs, whose input at
// spenderInputIndex is the one that took the HTLC to the second level. A
// script path spend carries more than one witness element, so the spend is
// not taken for our own revocation spend.
func secondLevelSpend(index int, spenderInputIndex uint32,
	spendingTx *wire.MsgTx) spend {

	for len(spendingTx.TxIn) <= int(spenderInputIndex) {
		spendingTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Index: uint32(len(spendingTx.TxIn)),
			},
		})
	}
	spendingTx.TxIn[spenderInputIndex].Witness = wire.TxWitness{
		{1}, {2}, {3},
	}

	txHash := spendingTx.TxHash()

	return spend{
		index: index,
		detail: &chainntnfs.SpendDetail{
			SpendingTx:        spendingTx,
			SpenderTxHash:     &txHash,
			SpenderInputIndex: spenderInputIndex,
			SpendingHeight:    500,
		},
	}
}

// auxTestResolver is an AuxContractResolver that records the requests it
// receives and answers them with a canned blob or error.
type auxTestResolver struct {
	reqs []lnwallet.ResolutionReq
	blob tlv.Blob
	err  error
}

// ResolveContract records the request and returns the canned answer.
func (r *auxTestResolver) ResolveContract(
	req lnwallet.ResolutionReq) fn.Result[tlv.Blob] {

	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return fn.Err[tlv.Blob](r.err)
	}

	return fn.Ok(r.blob)
}

// TestFindSecondLevelOutputIndex verifies that findSecondLevelOutputIndex
// correctly identifies the HTLC output in second-level transactions, including
// batched txs with multiple inputs and a wallet change output where input
// indices diverge from output indices, and transactions in which several
// outputs carry the same second-level script.
func TestFindSecondLevelOutputIndex(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)
	tapTweak := randomTapTweak(t)
	expectedPkScript := keys.secondLevelPkScript(t, tapTweak)
	unrelatedPkScript := randomPkScript(t)

	bo := keys.htlcOutput(
		t, 0, input.TaprootHtlcAcceptedRevoke, tapTweak, true,
	)

	txOut := func(pkScript []byte, value int64) *wire.TxOut {
		return &wire.TxOut{PkScript: pkScript, Value: value}
	}

	tests := []struct {
		name              string
		outputs           []*wire.TxOut
		spenderInputIndex uint32
		claimed           []uint32
		expectedIndex     uint32
		expectErr         bool
	}{{
		// Simple 1-input-1-output second-level tx.
		name:    "single output matches",
		outputs: []*wire.TxOut{txOut(expectedPkScript, 50_000)},
	}, {
		// Batched tx: wallet UTXO input adds a change output before
		// the HTLC output.
		name: "batched tx - HTLC at index 1",
		outputs: []*wire.TxOut{
			txOut(unrelatedPkScript, 100_000),
			txOut(expectedPkScript, 50_000),
		},
		expectedIndex: 1,
	}, {
		// Batched tx: HTLC output first, change output second, with
		// the HTLC spent by the second input.
		name: "batched tx - HTLC at index 0",
		outputs: []*wire.TxOut{
			txOut(expectedPkScript, 50_000),
			txOut(unrelatedPkScript, 100_000),
		},
		spenderInputIndex: 1,
	}, {
		// Batched tx with 3 outputs: HTLC in the middle.
		name: "batched tx - HTLC at index 1 of 3",
		outputs: []*wire.TxOut{
			txOut(unrelatedPkScript, 100_000),
			txOut(expectedPkScript, 50_000),
			txOut(unrelatedPkScript, 200_000),
		},
		spenderInputIndex: 2,
		expectedIndex:     1,
	}, {
		// Two HTLCs with the same second-level script in one tx that
		// pairs inputs with outputs: the output at the index of the
		// spending input is the one that belongs to the HTLC.
		name: "same script twice - spender index wins",
		outputs: []*wire.TxOut{
			txOut(expectedPkScript, 50_000),
			txOut(expectedPkScript, 60_000),
		},
		spenderInputIndex: 1,
		expectedIndex:     1,
	}, {
		// The output at the spender index was already assigned to
		// another breached output, so the next match is taken.
		name: "same script twice - claimed output skipped",
		outputs: []*wire.TxOut{
			txOut(expectedPkScript, 50_000),
			txOut(expectedPkScript, 60_000),
		},
		claimed:       []uint32{0},
		expectedIndex: 1,
	}, {
		// Every matching output is taken: nothing is left for this
		// HTLC, and it must not be mapped onto one of them a second
		// time.
		name: "same script twice - all claimed",
		outputs: []*wire.TxOut{
			txOut(expectedPkScript, 50_000),
			txOut(expectedPkScript, 60_000),
		},
		claimed:   []uint32{0, 1},
		expectErr: true,
	}, {
		// No matching output: should error.
		name:      "no match",
		outputs:   []*wire.TxOut{txOut(unrelatedPkScript, 50_000)},
		expectErr: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := &wire.MsgTx{TxOut: tc.outputs}
			txHash := tx.TxHash()
			claimed := func(op wire.OutPoint) bool {
				require.Equal(t, txHash, op.Hash)

				for _, idx := range tc.claimed {
					if op.Index == idx {
						return true
					}
				}

				return false
			}

			idx, err := findSecondLevelOutputIndex(
				&bo, tx, tc.spenderInputIndex, claimed,
			)
			if tc.expectErr {
				require.ErrorIs(
					t, err, errSecondLevelOutputNotFound,
				)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedIndex, idx)
		})
	}
}

// TestConvertToSecondLevelRevokeAuxGate asserts which output of the
// second-level transaction a breached HTLC is morphed onto. For an aux
// channel HTLC the output is located by its script, as the transaction may
// carry inputs and outputs that shift the indexes. For any other channel the
// output at the index of the spending input is used, exactly as before, even
// if a script match would point elsewhere.
func TestConvertToSecondLevelRevokeAuxGate(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)
	tapTweak := randomTapTweak(t)
	secondLevelScript := keys.secondLevelPkScript(t, tapTweak)
	otherScript := randomPkScript(t)

	// The HTLC is spent by input 0, but its second-level output sits at
	// index 1.
	newSpend := func() spend {
		return secondLevelSpend(0, 0, &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{PkScript: otherScript, Value: 700},
				{PkScript: secondLevelScript, Value: 900},
			},
		})
	}

	for _, aux := range []bool{true, false} {
		bo := keys.htlcOutput(
			t, 0, input.TaprootHtlcAcceptedRevoke, tapTweak, aux,
		)
		breachInfo := &retributionInfo{
			breachedOutputs: []breachedOutput{bo},
		}
		resolver := &auxTestResolver{blob: tlv.Blob{0x02}}

		s := newSpend()
		err := convertToSecondLevelRevoke(
			&breachInfo.breachedOutputs[0], breachInfo, s.detail,
			fn.Some[lnwallet.AuxContractResolver](resolver),
		)
		require.NoError(t, err)

		converted := breachInfo.breachedOutputs[0]
		require.Equal(
			t, input.TaprootHtlcSecondLevelRevoke,
			converted.witnessType,
		)

		wantIndex, wantAmt, wantScript := uint32(0), int64(700),
			otherScript
		if aux {
			wantIndex, wantAmt, wantScript = 1, 900,
				secondLevelScript
		}
		require.Equal(t, wire.OutPoint{
			Hash:  s.detail.SpendingTx.TxHash(),
			Index: wantIndex,
		}, converted.outpoint)
		require.EqualValues(t, wantAmt, converted.amt)
		require.Equal(t, wantAmt, converted.signDesc.Output.Value)
		require.Equal(
			t, wantScript, converted.signDesc.Output.PkScript,
		)

		// Only an aux channel HTLC is resolved again.
		if !aux {
			require.Empty(t, resolver.reqs)
			require.True(t, converted.resolutionBlob.IsNone())

			continue
		}

		// The request targets the second-level revocation of the
		// same HTLC, direction included, and carries the confirmed
		// second-level transaction.
		require.Len(t, resolver.reqs, 1)
		req := resolver.reqs[0]
		require.Equal(
			t, input.WitnessType(
				input.TaprootHtlcSecondLevelRevoke,
			), req.Type,
		)
		require.Equal(t, fn.Some(true), req.HtlcIncoming)
		require.Equal(t, fn.Some(uint64(0)), req.HtlcID)

		secondLevel, err := req.SecondLevel.UnwrapOrErr(
			errors.New("no second-level info"),
		)
		require.NoError(t, err)
		require.Equal(t, s.detail.SpendingTx, secondLevel.Tx)
		require.EqualValues(t, 500, secondLevel.BlockHeight)

		// The blob resolved for the commitment output is replaced by
		// the one resolved for the second-level output.
		require.Equal(
			t, fn.Some(tlv.Blob{0x02}), converted.resolutionBlob,
		)

		// The template itself is left untouched for later use.
		require.Equal(
			t, input.WitnessType(input.TaprootHtlcAcceptedRevoke),
			bo.resolveReq.Type,
		)
		require.True(t, bo.resolveReq.SecondLevel.IsNone())
	}
}

// TestConvertToSecondLevelRevokeResolveFailure asserts that a failing aux
// resolver does not hold up the conversion of an HTLC that was taken to the
// second level. The output is still morphed, so its BTC value is swept, but
// the blob that was resolved for the commitment output is dropped: carried
// along, it would make every justice transaction that includes the output
// invalid for the aux sweeper and stall the whole breach.
func TestConvertToSecondLevelRevokeResolveFailure(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)
	tapTweak := randomTapTweak(t)
	bo := keys.htlcOutput(
		t, 0, input.TaprootHtlcOfferedRevoke, tapTweak, true,
	)
	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{bo},
	}
	require.True(t, bo.resolutionBlob.IsSome())

	s := secondLevelSpend(0, 0, &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			PkScript: keys.secondLevelPkScript(t, tapTweak),
			Value:    900,
		}},
	})
	resolver := &auxTestResolver{err: errors.New("cannot resolve")}

	err := convertToSecondLevelRevoke(
		&breachInfo.breachedOutputs[0], breachInfo, s.detail,
		fn.Some[lnwallet.AuxContractResolver](resolver),
	)
	require.NoError(t, err)
	require.Len(t, resolver.reqs, 1)

	converted := breachInfo.breachedOutputs[0]
	require.Equal(
		t, input.TaprootHtlcSecondLevelRevoke, converted.witnessType,
	)
	require.EqualValues(t, 900, converted.amt)
	require.True(t, converted.resolutionBlob.IsNone())
}

// TestUpdateBreachInfoSecondLevelOutputNotFound asserts what happens when
// the second-level output of an aux channel HTLC cannot be located in the
// transaction that spent it. The breached output must be left exactly as it
// was, not half converted, and it is then dropped from the breach without
// its value being reported as claimed: its outpoint is spent, so keeping it
// would have the next spend notification replay the very same spend, and
// counting it would report funds as recovered that were not.
func TestUpdateBreachInfoSecondLevelOutputNotFound(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)
	lost := keys.htlcOutput(
		t, 1, input.TaprootHtlcAcceptedRevoke, randomTapTweak(t), true,
	)
	keptTweak := randomTapTweak(t)
	kept := keys.htlcOutput(
		t, 2, input.TaprootHtlcOfferedRevoke, keptTweak, true,
	)

	// The transaction that spends the first HTLC has no output with the
	// expected second-level script.
	lostSpend := secondLevelSpend(0, 0, &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			PkScript: randomPkScript(t), Value: 900,
		}},
	})

	// Converting it directly fails without touching the output.
	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{lost, kept},
	}
	resolver := &auxTestResolver{blob: tlv.Blob{0x02}}
	auxResolver := fn.Some[lnwallet.AuxContractResolver](resolver)

	err := convertToSecondLevelRevoke(
		&breachInfo.breachedOutputs[0], breachInfo, lostSpend.detail,
		auxResolver,
	)
	require.ErrorIs(t, err, errSecondLevelOutputNotFound)
	require.Equal(t, lost, breachInfo.breachedOutputs[0])
	require.Empty(t, resolver.reqs)

	// Applied as a spend, the output is dropped and nothing is counted.
	// The other HTLC, spent to the second level properly in the same
	// batch, is converted as usual.
	keptSpend := secondLevelSpend(1, 0, &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			PkScript: keys.secondLevelPkScript(t, keptTweak),
			Value:    800,
		}},
	})
	total, revoked := updateBreachInfo(
		breachInfo, []spend{lostSpend, keptSpend}, auxResolver,
	)
	require.Zero(t, total)
	require.Zero(t, revoked)

	require.Len(t, breachInfo.breachedOutputs, 1)
	remaining := breachInfo.breachedOutputs[0]
	require.Equal(
		t, input.TaprootHtlcSecondLevelRevoke, remaining.witnessType,
	)
	require.EqualValues(t, 800, remaining.amt)
	require.Equal(
		t, keptSpend.detail.SpendingTx.TxHash(),
		remaining.outpoint.Hash,
	)
}

// TestUpdateBreachInfoSharedSecondLevelScript asserts that two aux channel
// HTLCs whose second-level outputs share a script, bundled into one
// second-level transaction, are morphed onto two different outputs. Mapping
// both onto the first match would put the same outpoint into the justice
// transaction twice, which makes it invalid and stalls the whole breach.
func TestUpdateBreachInfoSharedSecondLevelScript(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)

	// The second-level script depends on keys, the CSV delay and the aux
	// leaf only, so two HTLCs with the same aux leaf share it.
	tapTweak := randomTapTweak(t)
	sharedScript := keys.secondLevelPkScript(t, tapTweak)

	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{
			keys.htlcOutput(
				t, 1, input.TaprootHtlcAcceptedRevoke,
				tapTweak, true,
			),
			keys.htlcOutput(
				t, 2, input.TaprootHtlcAcceptedRevoke,
				tapTweak, true,
			),
		},
	}

	// One transaction takes both HTLCs to the second level, the first
	// HTLC through its second input and vice versa.
	bundle := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{PkScript: sharedScript, Value: 900},
			{PkScript: sharedScript, Value: 800},
		},
	}
	spends := []spend{
		secondLevelSpend(0, 1, bundle),
		secondLevelSpend(1, 0, bundle),
	}
	require.Equal(
		t, spends[0].detail.SpendingTx, spends[1].detail.SpendingTx,
	)

	resolver := &auxTestResolver{blob: tlv.Blob{0x02}}
	updateBreachInfo(
		breachInfo, spends,
		fn.Some[lnwallet.AuxContractResolver](resolver),
	)

	require.Len(t, breachInfo.breachedOutputs, 2)
	first, second := breachInfo.breachedOutputs[0],
		breachInfo.breachedOutputs[1]

	// Each HTLC follows the input that spent it.
	bundleHash := bundle.TxHash()
	require.Equal(
		t, wire.OutPoint{Hash: bundleHash, Index: 1}, first.outpoint,
	)
	require.EqualValues(t, 800, first.amt)
	require.Equal(
		t, wire.OutPoint{Hash: bundleHash, Index: 0}, second.outpoint,
	)
	require.EqualValues(t, 900, second.amt)
}

// mockAuxSweeperWithOutput is a mock AuxSweeper that returns a configurable
// sweep output from DeriveSweepAddr.
type mockAuxSweeperWithOutput struct {
	outputValue int64
	pkScript    []byte
}

func (m *mockAuxSweeperWithOutput) DeriveSweepAddr(
	_ []input.Input,
	_ lnwallet.AddrWithKey) fn.Result[sweep.SweepOutput] {

	return fn.Ok(sweep.SweepOutput{
		TxOut: wire.TxOut{
			PkScript: m.pkScript,
			Value:    m.outputValue,
		},
	})
}

func (m *mockAuxSweeperWithOutput) ExtraBudgetForInputs(
	_ []input.Input) fn.Result[btcutil.Amount] {

	return fn.Ok(btcutil.Amount(0))
}

func (m *mockAuxSweeperWithOutput) NotifyBroadcast(
	_ *sweep.BumpRequest, _ *wire.MsgTx,
	_ btcutil.Amount, _ map[wire.OutPoint]int,
	_ sweep.AuxNotifyOpts) error {

	return nil
}

// TestSweepSpendableOutputsAuxOutput tests the output construction logic in
// sweepSpendableOutputsTxn. Without an aux output the transaction is built
// exactly as for any regular channel. With one, the BTC sweep output is only
// added above the dust limit, and what the inputs hold beyond the aux output
// is otherwise paid as fees.
func TestSweepSpendableOutputsAuxOutput(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript, err := input.PayToTaprootScript(privKey.PubKey())
	require.NoError(t, err)

	// Build a P2WKH pkScript for the input (WitnessKeyHash needs
	// a valid P2WKH output to sign).
	p2wkhAddr := address.Hash160(
		privKey.PubKey().SerializeCompressed(),
	)
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0)
	builder.AddData(p2wkhAddr)
	p2wkhScript, err := builder.Script()
	require.NoError(t, err)

	makeOutput := func(value int64) breachedOutput {
		return breachedOutput{
			amt: btcutil.Amount(value),
			outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x01},
				Index: 0,
			},
			witnessType: input.WitnessKeyHash,
			signDesc: input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					PubKey: privKey.PubKey(),
				},
				Output: &wire.TxOut{
					PkScript: p2wkhScript,
					Value:    value,
				},
			},
		}
	}

	sweepAddr := lnwallet.AddrWithKey{
		DeliveryAddress: pkScript,
	}

	signer := input.NewMockSigner(
		[]*btcec.PrivateKey{privKey},
		&chaincfg.RegressionNetParams,
	)

	// The fee rate is fixed, so the fee follows from the weight alone.
	feeRate := chainfee.FeePerKwFloor
	makeBrar := func(auxOutValue int64) *BreachArbitrator {
		genScript := func() fn.Result[lnwallet.AddrWithKey] {
			return fn.Ok(sweepAddr)
		}

		auxSweeper := fn.None[sweep.AuxSweeper]()
		if auxOutValue > 0 {
			auxSweeper = fn.Some[sweep.AuxSweeper](
				&mockAuxSweeperWithOutput{
					outputValue: auxOutValue,
					pkScript:    pkScript,
				},
			)
		}

		return &BreachArbitrator{
			cfg: &BreachConfig{
				Signer:         signer,
				GenSweepScript: genScript,
				Estimator: chainfee.NewStaticEstimator(
					feeRate, 0,
				),
				AuxSweeper: auxSweeper,
			},
		}
	}

	const txWeight = 400
	estimatedFee := int64(feeRate.FeeForWeight(txWeight))
	dustLimit := int64(
		lnwallet.DustLimitForSize(input.UnknownWitnessSize),
	)

	testCases := []struct {
		name        string
		inputValue  int64
		auxOutValue int64
		wantOutputs []int64
		wantFee     int64
		wantErr     string
	}{{
		name:        "no aux output",
		inputValue:  1_000_000,
		wantOutputs: []int64{1_000_000 - estimatedFee},
		wantFee:     estimatedFee,
	}, {
		// A regular channel gets its sweep output no matter how
		// small it is, exactly as before.
		name:        "no aux output, sweep output below dust",
		inputValue:  estimatedFee + dustLimit - 1,
		wantOutputs: []int64{dustLimit - 1},
		wantFee:     estimatedFee,
	}, {
		// And an input that cannot pay the fee still fails on the
		// negative output.
		name:       "no aux output, negative sweep output",
		inputValue: estimatedFee - 1,
		wantErr:    "negative",
	}, {
		name:        "aux output and sweep output",
		inputValue:  1_000_000,
		auxOutValue: 10_000,
		wantOutputs: []int64{
			10_000, 1_000_000 - 10_000 - estimatedFee,
		},
		wantFee: estimatedFee,
	}, {
		// The sweep output would sit exactly at the dust limit: it
		// is dropped and its value goes to fees.
		name:        "aux output, sweep output at dust limit",
		inputValue:  400 + estimatedFee + dustLimit,
		auxOutValue: 400,
		wantOutputs: []int64{400},
		wantFee:     estimatedFee + dustLimit,
	}, {
		// The inputs cannot even pay the estimated fee on top of the
		// aux output: what is left beyond the aux output is the fee.
		name:        "aux output, fee below the estimate",
		inputValue:  630,
		auxOutValue: 400,
		wantOutputs: []int64{400},
		wantFee:     230,
	}, {
		// The inputs cannot fund the aux output at all: the
		// transaction would spend more than it has.
		name:        "aux output larger than the inputs",
		inputValue:  354,
		auxOutValue: 1_000,
		wantErr:     "cannot fund the aux sweep output",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			brar := makeBrar(tc.auxOutValue)
			bo := makeOutput(tc.inputValue)

			result, err := brar.sweepSpendableOutputsTxn(
				txWeight, &bo,
			)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)

			var (
				outputs  []int64
				totalOut int64
			)
			for _, txOut := range result.justiceTx.TxOut {
				outputs = append(outputs, txOut.Value)
				totalOut += txOut.Value
			}
			require.Equal(t, tc.wantOutputs, outputs)

			// The reported fee is what the transaction really
			// pays.
			require.EqualValues(t, tc.wantFee, result.fee)
			require.Equal(t, tc.inputValue-totalOut, tc.wantFee)
		})
	}
}

// mockAuxSweeperNotify is a mock implementation of sweep.AuxSweeper that tracks
// calls to NotifyBroadcast for testing purposes.
type mockAuxSweeperNotify struct {
	notifyCalls []notifyCall
	notifyErr   error
}

// notifyCall records the parameters of a NotifyBroadcast call.
type notifyCall struct {
	req  *sweep.BumpRequest
	tx   *wire.MsgTx
	fee  btcutil.Amount
	opts sweep.AuxNotifyOpts
}

// DeriveSweepAddr implements sweep.AuxSweeper.
func (m *mockAuxSweeperNotify) DeriveSweepAddr(_ []input.Input,
	_ lnwallet.AddrWithKey) fn.Result[sweep.SweepOutput] {

	return fn.Ok(sweep.SweepOutput{})
}

// ExtraBudgetForInputs implements sweep.AuxSweeper.
func (m *mockAuxSweeperNotify) ExtraBudgetForInputs(
	_ []input.Input) fn.Result[btcutil.Amount] {

	return fn.Ok(btcutil.Amount(0))
}

// NotifyBroadcast implements sweep.AuxSweeper and records the call.
func (m *mockAuxSweeperNotify) NotifyBroadcast(req *sweep.BumpRequest,
	tx *wire.MsgTx, fee btcutil.Amount,
	_ map[wire.OutPoint]int, opts sweep.AuxNotifyOpts) error {

	m.notifyCalls = append(m.notifyCalls, notifyCall{
		req:  req,
		tx:   tx,
		fee:  fee,
		opts: opts,
	})

	return m.notifyErr
}

// justiceTrackerHarness bundles a justice tx tracker with a breach arbiter
// whose aux sweeper records the notifications it receives.
type justiceTrackerHarness struct {
	t *testing.T

	brar    *BreachArbitrator
	sweeper *mockAuxSweeperNotify
	tracker *justiceTxTracker
}

// newJusticeTrackerHarness creates a harness whose tracker is seeded with
// the given justice txs, as if they had been recorded before a restart.
func newJusticeTrackerHarness(t *testing.T,
	recorded map[chainhash.Hash]*justiceTxCtx) *justiceTrackerHarness {

	t.Helper()

	sweeper := &mockAuxSweeperNotify{}

	return &justiceTrackerHarness{
		t: t,
		brar: &BreachArbitrator{
			cfg: &BreachConfig{
				AuxSweeper: fn.Some[sweep.AuxSweeper](sweeper),
			},
		},
		sweeper: sweeper,
		tracker: newJusticeTxTracker(recorded),
	}
}

// spendsOf processes the spend of the breached outputs at the given indexes
// by the given transaction the way the retribution loop does: the confirmed
// justice txs are collected first, then the outputs the transaction spent
// are removed, then the aux sweeper is notified.
func (h *justiceTrackerHarness) spendsOf(breachInfo *retributionInfo,
	tx *wire.MsgTx, indexes ...int) {

	h.t.Helper()

	txHash := tx.TxHash()
	spends := make([]spend, 0, len(indexes))
	for _, index := range indexes {
		spends = append(spends, spend{
			index: index,
			detail: &chainntnfs.SpendDetail{
				SpendingTx:     tx,
				SpenderTxHash:  &txHash,
				SpendingHeight: 700,
			},
		})
	}

	h.tracker.collect(spends, breachInfo.breachedOutputs)

	// Drop the spent outputs, like updateBreachInfo does for outputs
	// that reached their terminal state.
	spent := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		spent[index] = struct{}{}
	}
	var remaining []breachedOutput
	for i := range breachInfo.breachedOutputs {
		if _, ok := spent[i]; !ok {
			remaining = append(
				remaining, breachInfo.breachedOutputs[i],
			)
		}
	}
	breachInfo.breachedOutputs = remaining

	h.brar.notifyConfirmedJusticeTxs(h.tracker)
}

// newJusticeTx returns a justice tx spending the given breached outputs,
// along with its context.
func newJusticeTx(fee btcutil.Amount,
	outputs ...breachedOutput) *justiceTxCtx {

	tx := wire.NewMsgTx(2)
	for i := range outputs {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outputs[i].outpoint,
		})
	}
	tx.AddTxOut(&wire.TxOut{Value: int64(fee)})

	return &justiceTxCtx{
		justiceTx: tx,
		fee:       fee,
		sweepAddr: lnwallet.AddrWithKey{
			DeliveryAddress: []byte{0x51, byte(fee)},
		},
	}
}

// commitOutput returns a breached commitment output at the given index.
func commitOutput(index uint32) breachedOutput {
	return breachedOutput{
		amt:            5000,
		outpoint:       wire.OutPoint{Index: index},
		witnessType:    input.TaprootCommitmentRevoke,
		resolutionBlob: fn.Some(tlv.Blob{0xc0, byte(index)}),
	}
}

// TestJusticeTxTrackerNotifiesOnce asserts that the aux sweeper is told
// about a confirmed justice tx exactly once and with all of its inputs, even
// though the spends of its inputs are reported one at a time: each breached
// output has its own spend subscription, and the retribution loop moves on
// as soon as the first one fires.
func TestJusticeTxTrackerNotifiesOnce(t *testing.T) {
	t.Parallel()

	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{
			commitOutput(0), commitOutput(1), commitOutput(2),
		},
	}
	justice := newJusticeTx(1000, breachInfo.breachedOutputs...)
	variants := &justiceTxVariants{spendAll: justice}

	h := newJusticeTrackerHarness(t, nil)
	added := h.tracker.record(variants)
	require.Len(t, added, 1)

	// Recording the same variants again adds nothing.
	require.Empty(t, h.tracker.record(variants))

	// The spend of the first input is reported alone. All three outputs
	// are still part of the breach, so the notification is complete.
	h.spendsOf(breachInfo, justice.justiceTx, 0)
	require.Len(t, h.sweeper.notifyCalls, 1)
	require.False(t, h.tracker.hasPending())

	call := h.sweeper.notifyCalls[0]
	require.Equal(t, justice.justiceTx, call.tx)
	require.EqualValues(t, 1000, call.fee)
	require.Equal(t, justice.sweepAddr, call.req.DeliveryAddress)
	require.Len(t, call.req.Inputs, 3)
	for i, inp := range call.req.Inputs {
		wantOp := wire.OutPoint{Index: uint32(i)}
		require.Equal(t, wantOp, inp.OutPoint())
		require.Equal(
			t, fn.Some(tlv.Blob{0xc0, byte(i)}),
			inp.ResolutionBlob(),
		)
	}

	// The transaction is confirmed: the aux sweeper must not broadcast
	// it, and it is told where to look for it.
	require.True(t, call.opts.SkipBroadcast)
	require.EqualValues(t, 700, call.opts.ConfirmHeight)
	require.False(t, call.opts.LookupInputProofs)

	// The spends of the other two inputs follow in later batches. They
	// must not trigger a second, partial notification.
	h.spendsOf(breachInfo, justice.justiceTx, 0)
	h.spendsOf(breachInfo, justice.justiceTx, 0)
	require.Empty(t, breachInfo.breachedOutputs)
	require.Len(t, h.sweeper.notifyCalls, 1)
}

// TestJusticeTxTrackerIgnoresForeignSpends asserts that a spend by a
// transaction that is not one of our justice txs, such as the remote party
// taking an HTLC to the second level, is never reported to the aux sweeper.
func TestJusticeTxTrackerIgnoresForeignSpends(t *testing.T) {
	t.Parallel()

	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{
			commitOutput(0), commitOutput(1),
		},
	}
	justice := newJusticeTx(1000, breachInfo.breachedOutputs...)

	h := newJusticeTrackerHarness(t, nil)
	h.tracker.record(&justiceTxVariants{spendAll: justice})

	foreign := newJusticeTx(77, breachInfo.breachedOutputs[1])
	h.spendsOf(breachInfo, foreign.justiceTx, 1)

	require.Empty(t, h.sweeper.notifyCalls)
	require.False(t, h.tracker.hasPending())
}

// TestJusticeTxTrackerRetriesFailedNotify asserts that a notification the
// aux sweeper fails to take is not lost. The confirmed justice tx stays
// pending, which keeps the breach from being cleaned up, and the
// notification is retried with the same inputs until it goes through.
func TestJusticeTxTrackerRetriesFailedNotify(t *testing.T) {
	t.Parallel()

	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{
			commitOutput(0), commitOutput(1),
		},
	}
	justice := newJusticeTx(1000, breachInfo.breachedOutputs...)

	h := newJusticeTrackerHarness(t, nil)
	h.tracker.record(&justiceTxVariants{spendAll: justice})

	// The aux sweeper is unable to take the notification.
	h.sweeper.notifyErr = errors.New("aux sweeper unavailable")
	h.spendsOf(breachInfo, justice.justiceTx, 0, 1)
	require.Empty(t, breachInfo.breachedOutputs)
	require.Len(t, h.sweeper.notifyCalls, 1)
	require.True(t, h.tracker.hasPending())

	// It is retried, as the retribution loop does on every block, and
	// still fails.
	h.brar.notifyConfirmedJusticeTxs(h.tracker)
	require.Len(t, h.sweeper.notifyCalls, 2)
	require.True(t, h.tracker.hasPending())

	// Once the aux sweeper recovers, the notification goes through with
	// the outputs the transaction spent, although they have long been
	// removed from the breach.
	h.sweeper.notifyErr = nil
	h.brar.notifyConfirmedJusticeTxs(h.tracker)
	require.Len(t, h.sweeper.notifyCalls, 3)
	require.Len(t, h.sweeper.notifyCalls[2].req.Inputs, 2)
	require.False(t, h.tracker.hasPending())

	// And it is not repeated after that.
	h.brar.notifyConfirmedJusticeTxs(h.tracker)
	require.Len(t, h.sweeper.notifyCalls, 3)
}

// TestJusticeTxTrackerAfterRestart asserts that a justice tx that was
// created before a restart is still recognized when it confirms afterwards.
// After a restart the spends are replayed from the breach on, and the justice
// txs that are built then differ from the ones that were broadcast before
// (new sweep address, new fee), so only the recorded history can match the
// confirmed transaction.
//
// The replay also reports the confirmed justice tx through its commitment
// input before the HTLC it spent on the second level has been morphed back
// into that second-level output. The aux sweeper must only be notified once
// both inputs are known, with both of them.
func TestJusticeTxTrackerAfterRestart(t *testing.T) {
	t.Parallel()

	keys := newAuxBreachTestKeys(t)
	tapTweak := randomTapTweak(t)

	// Before the restart: the HTLC was taken to the second level, and a
	// justice tx spending the commitment output and the second-level
	// output was broadcast.
	htlcSpend := secondLevelSpend(0, 0, &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			PkScript: keys.secondLevelPkScript(t, tapTweak),
			Value:    900,
		}},
	})
	secondLevelOp := wire.OutPoint{
		Hash: htlcSpend.detail.SpendingTx.TxHash(),
	}

	commit := commitOutput(0)
	justice := newJusticeTx(1000, commit, breachedOutput{
		outpoint: secondLevelOp,
	})
	justice.hasSecondLevel = true

	// Only the metadata survives the restart, keyed by txid.
	recorded := newJusticeTxTracker(nil).record(
		&justiceTxVariants{spendAll: justice},
	)
	require.Len(t, recorded, 1)
	for _, jtx := range recorded {
		require.Nil(t, jtx.justiceTx)
		require.Nil(t, jtx.inputs)
	}

	// After the restart the breach starts over with the outputs as they
	// were on the commitment.
	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{
			commit,
			keys.htlcOutput(
				t, 1, input.TaprootHtlcAcceptedRevoke,
				tapTweak, true,
			),
		},
	}
	h := newJusticeTrackerHarness(t, recorded)

	// The spend of the commitment output by the justice tx is replayed
	// first. The HTLC still sits on the commitment, so one input of the
	// justice tx is unknown and nothing is reported yet.
	h.spendsOf(breachInfo, justice.justiceTx, 0)
	require.Empty(t, h.sweeper.notifyCalls)
	require.True(t, h.tracker.hasPending())

	// Then the HTLC's spend to the second level is replayed, which
	// morphs it onto the second-level output.
	resolver := &auxTestResolver{blob: tlv.Blob{0x02}}
	updateBreachInfo(
		breachInfo, []spend{htlcSpend},
		fn.Some[lnwallet.AuxContractResolver](resolver),
	)
	require.Len(t, breachInfo.breachedOutputs, 1)
	require.Equal(
		t, secondLevelOp, breachInfo.breachedOutputs[0].outpoint,
	)

	// Finally the spend of the second-level output by the justice tx is
	// replayed. Now the notification is complete.
	h.spendsOf(breachInfo, justice.justiceTx, 0)
	require.Len(t, h.sweeper.notifyCalls, 1)
	require.False(t, h.tracker.hasPending())

	call := h.sweeper.notifyCalls[0]
	require.Equal(t, justice.sweepAddr, call.req.DeliveryAddress)
	require.EqualValues(t, 1000, call.fee)
	require.Len(t, call.req.Inputs, 2)
	require.Equal(t, commit.outpoint, call.req.Inputs[0].OutPoint())
	require.Equal(t, secondLevelOp, call.req.Inputs[1].OutPoint())

	// The second-level input carries the blob that was resolved for it
	// after the restart, and its proof has to be looked up.
	require.Equal(
		t, fn.Some(tlv.Blob{0x02}),
		call.req.Inputs[1].ResolutionBlob(),
	)
	require.True(t, call.opts.LookupInputProofs)
}

// TestJusticeTxTrackerNoAuxSweeper asserts that confirmed justice txs are
// simply dropped from the pending set when no aux sweeper is configured, so
// they can never hold up the cleanup of a breach.
func TestJusticeTxTrackerNoAuxSweeper(t *testing.T) {
	t.Parallel()

	breachInfo := &retributionInfo{
		breachedOutputs: []breachedOutput{commitOutput(0)},
	}
	justice := newJusticeTx(1000, breachInfo.breachedOutputs...)

	h := newJusticeTrackerHarness(t, nil)
	h.brar.cfg.AuxSweeper = fn.None[sweep.AuxSweeper]()
	h.tracker.record(&justiceTxVariants{spendAll: justice})

	h.spendsOf(breachInfo, justice.justiceTx, 0)
	require.False(t, h.tracker.hasPending())
}
