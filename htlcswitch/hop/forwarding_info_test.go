package hop

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidateFinalHtlc exercises the final-hop HTLC validation helper.
func TestValidateFinalHtlc(t *testing.T) {
	t.Parallel()

	const (
		amount       = lnwire.MilliSatoshi(1000)
		expiry       = uint32(150)
		height       = uint32(100)
		maxCltvDelta = uint32(50)
	)

	fwdInfo := ForwardingInfo{
		AmountToForward: amount,
		OutgoingCLTV:    expiry,
		NextHop:         Exit,
	}

	testCases := []struct {
		name           string
		amount         lnwire.MilliSatoshi
		expiry         uint32
		height         uint32
		maxCltvDelta   uint32
		fwdInfo        ForwardingInfo
		validateAmount bool
		expected       FinalHtlcValidationResult
	}{{
		name:           "valid",
		amount:         amount,
		expiry:         expiry,
		height:         height + 1,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcValid,
	}, {
		name:           "amount too low",
		amount:         amount - 1,
		expiry:         expiry,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcInvalidAmount,
	}, {
		name:           "amount check disabled",
		amount:         amount - 1,
		expiry:         expiry,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: false,
		expected:       FinalHtlcValid,
	}, {
		name:           "final cltv too low",
		amount:         amount,
		expiry:         expiry - 1,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcInvalidCltv,
	}, {
		name:           "expiry too far",
		amount:         amount,
		expiry:         expiry + 1,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcExpiryTooFar,
	}, {
		name:           "expiry at maximum",
		amount:         amount,
		expiry:         expiry,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcValid,
	}, {
		name:           "height above expiry",
		amount:         amount,
		expiry:         expiry,
		height:         expiry + 1,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcValid,
	}, {
		name:           "amount failure takes precedence",
		amount:         amount - 1,
		expiry:         expiry - 1,
		height:         height,
		maxCltvDelta:   maxCltvDelta,
		fwdInfo:        fwdInfo,
		validateAmount: true,
		expected:       FinalHtlcInvalidAmount,
	}, {
		name: "cltv failure takes precedence over " +
			"expiry too far",
		amount:       amount,
		expiry:       expiry + maxCltvDelta + 1,
		height:       height,
		maxCltvDelta: maxCltvDelta,
		fwdInfo: ForwardingInfo{
			AmountToForward: amount,
			OutgoingCLTV:    expiry + maxCltvDelta + 2,
			NextHop:         Exit,
		},
		validateAmount: true,
		expected:       FinalHtlcInvalidCltv,
	}}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := ValidateFinalHtlc(
				testCase.amount, testCase.expiry,
				testCase.height, testCase.maxCltvDelta,
				testCase.fwdInfo, testCase.validateAmount,
			)

			require.Equal(t, testCase.expected, result)
		})
	}
}
