package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
)

var (
	witnessTypes = []input.WitnessType{
		input.CommitmentTimeLock,
		input.HtlcAcceptedSuccessSecondLevel,
		input.HtlcOfferedRemoteTimeout,
		input.WitnessKeyHash,
	}
	expectedWeight  = int64(1460)
	expectedSummary = "0000000000000000000000000000000000000000000000000000000000000000:10 (CommitmentTimeLock), " +
		"0000000000000000000000000000000000000000000000000000000000000001:11 (HtlcAcceptedSuccessSecondLevel), " +
		"0000000000000000000000000000000000000000000000000000000000000002:12 (HtlcOfferedRemoteTimeout), " +
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

	_, estimator := getWeightEstimate(inputs, nil, 0)
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
