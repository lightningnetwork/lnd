package chainntnfs_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// TestSpendFinalityValidation verifies invalid confirmation depths are
// rejected by the shared policy.
func TestSpendFinalityValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		numConfs uint32
	}{
		{name: "zero depth"},
		{
			name:     "excess depth",
			numConfs: chainntnfs.MaxNumConfs + 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := chainntnfs.NewSpendFinality(testCase.numConfs)
			require.ErrorIs(
				t, err, chainntnfs.ErrNumConfsOutOfRange,
			)
		})
	}
}

// TestSpendFinalityClassification verifies spend heights are classified
// against the configured confirmation depth.
func TestSpendFinalityClassification(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		numConfs      uint32
		spendHeight   int32
		currentHeight int32
		final         bool
	}{
		{
			name: "future spend", numConfs: 1, spendHeight: 11,
			currentHeight: 10,
		},
		{
			name: "one confirmation", numConfs: 1, spendHeight: 10,
			currentHeight: 10, final: true,
		},
		{
			name: "below depth", numConfs: 3, spendHeight: 10,
			currentHeight: 11,
		},
		{
			name: "at depth", numConfs: 3, spendHeight: 10,
			currentHeight: 12, final: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			finality, err := chainntnfs.NewSpendFinality(
				testCase.numConfs,
			)
			require.NoError(t, err)
			require.Equal(t, testCase.final, finality.IsFinal(
				testCase.spendHeight, testCase.currentHeight,
			))
		})
	}
}
