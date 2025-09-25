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

// TestTypedFeeTypeDecodeInvalidLength ensures that decoding a Fee TLV
// with an invalid length (anything other than 8 bytes) fails with an error.
func TestTypedFeeTypeDecodeInvalidLength(t *testing.T) {
	t.Parallel()

	fee := Fee{
		BaseFee: 1, FeeRate: 1,
	}

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&fee))

	// Corrupt the TLV length field to simulate malformed input.
	extraData[3] = 8 + 1

	var out Fee
	_, err := extraData.ExtractRecords(&out)
	require.Error(t, err)

	extraData[3] = 8 - 1

	_, err = extraData.ExtractRecords(&out)
	require.Error(t, err)
}
