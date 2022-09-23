package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypedFee(t *testing.T) {
	t.Parallel()

	t.Run("positive", func(t *testing.T) {
		t.Parallel()

		testTypedFee(t, Fee{
			BaseFee: 10,
			FeeRate: 20,
		})
	})

	t.Run("negative", func(t *testing.T) {
		t.Parallel()

		testTypedFee(t, Fee{
			BaseFee: -10,
			FeeRate: -20,
		})
	})
}

func testTypedFee(t *testing.T, fee Fee) { //nolint: thelper
	var eob ExtraOpaqueData
	require.NoError(t, eob.PackRecords(&fee))

	var extractedFee Fee
	_, err := eob.ExtractRecords(&extractedFee)
	require.NoError(t, err)

	require.Equal(t, fee, extractedFee)
}
