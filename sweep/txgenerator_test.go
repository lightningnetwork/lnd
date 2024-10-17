package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

var (
	witnessTypes = []input.WitnessType{
		input.CommitmentTimeLock,
		input.HtlcAcceptedSuccessSecondLevel,
		input.HtlcOfferedRemoteTimeout,
		input.WitnessKeyHash,
	}
	expectedWeight = int64(1460)

	//nolint:ll
	expectedSummary = "0000000000000000000000000000000000000000000000000000000000000000:10 (CommitmentTimeLock)\n" +
		"0000000000000000000000000000000000000000000000000000000000000001:11 (HtlcAcceptedSuccessSecondLevel)\n" +
		"0000000000000000000000000000000000000000000000000000000000000002:12 (HtlcOfferedRemoteTimeout)\n" +
		"0000000000000000000000000000000000000000000000000000000000000003:13 (WitnessKeyHash)"
)

// TestWeightEstimate tests that the estimated weight and number of CSVs/CLTVs
// used is correct for a transaction that uses inputs with the witness types
// defined in witnessTypes.
func TestWeightEstimate(t *testing.T) {
	t.Parallel()

	var inputs []input.Input
	for i, witnessType := range witnessTypes {
		inputs = append(inputs, input.NewBaseInput(
			&wire.OutPoint{
				Hash:  chainhash.Hash{byte(i)},
				Index: uint32(i) + 10,
			}, witnessType,
			&input.SignDescriptor{}, 0,
		))
	}

	// Create a sweep script that is always fed into the weight estimator,
	// regardless if it's actually included in the tx. It will be a P2WKH
	// script.
	changePkScript := []byte{
		0x00, 0x14,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}

	_, estimator, err := getWeightEstimate(
		inputs, nil, 0, 0, [][]byte{changePkScript},
	)
	require.NoError(t, err)

	weight := int64(estimator.weight())
	if weight != expectedWeight {
		t.Fatalf("unexpected weight. expected %d but got %d.",
			expectedWeight, weight)
	}
	summary := inputTypeSummary(inputs)
	if summary != expectedSummary {
		t.Fatalf("unexpected summary. expected %s but got %s.",
			expectedSummary, summary)
	}
}

// TestWeightEstimatorUnknownScript tests that the weight estimator fails when
// given an unknown script and succeeds when given a known script.
func TestWeightEstimatorUnknownScript(t *testing.T) {
	tests := []struct {
		name       string
		pkscript   []byte
		expectFail bool
	}{
		{
			name: "p2tr output",
			pkscript: []byte{
				0x51, 0x20,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
		},
		{
			name: "p2wsh output",
			pkscript: []byte{
				0x00, 0x20,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
		},
		{
			name: "p2wkh output",
			pkscript: []byte{
				0x00, 0x14,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
		},
		{
			name: "p2pkh output",
			pkscript: []byte{
				0x76, 0xa9, 0x14,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
				0x88, 0xac,
			},
		},
		{
			name: "p2sh output",
			pkscript: []byte{
				0xa9, 0x14,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
				0x87,
			},
		},
		{
			name:       "unknown script",
			pkscript:   []byte{0x00},
			expectFail: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testUnknownScriptInner(
				t, test.pkscript, test.expectFail,
			)
		})
	}
}

func testUnknownScriptInner(t *testing.T, pkscript []byte, expectFail bool) {
	var inputs []input.Input
	for i, witnessType := range witnessTypes {
		inputs = append(inputs, input.NewBaseInput(
			&wire.OutPoint{
				Hash:  chainhash.Hash{byte(i)},
				Index: uint32(i) + 10,
			}, witnessType,
			&input.SignDescriptor{}, 0,
		))
	}

	_, _, err := getWeightEstimate(inputs, nil, 0, 0, [][]byte{pkscript})
	if expectFail {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
}
