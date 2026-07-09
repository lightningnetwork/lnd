package hop

import (
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
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
		NextHop:         NewChannelNextHop(Exit),
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
			NextHop:         NewChannelNextHop(Exit),
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

// TestForwardingInfoNextHop asserts the next-hop accessors for both the short
// channel ID (Left) and node ID (Right) representations, including the
// load-bearing invariant that the zero-value ForwardingInfo denotes the exit
// hop.
func TestForwardingInfoNextHop(t *testing.T) {
	t.Parallel()

	scid := lnwire.NewShortChanIDFromInt(12345)

	var nodeID [33]byte
	nodeID[0] = 0x02
	nodeID[1] = 0xff

	// The zero-value ForwardingInfo must denote the exit hop, since its
	// NextHop is a Left equal to hop.Exit. Callers rely on this to detect
	// that we are the final recipient.
	zero := ForwardingInfo{}
	require.True(t, zero.IsExit(), "zero value must be the exit hop")
	require.Equal(
		t, fn.Some(Exit), zero.NextHopChannel(),
		"zero value must expose the Exit channel",
	)

	// An explicit channel next hop equal to Exit is likewise the exit hop.
	exit := ForwardingInfo{NextHop: NewChannelNextHop(Exit)}
	require.True(t, exit.IsExit())

	// A channel next hop with a real SCID is a forward, and exposes that
	// SCID through NextHopChannel.
	channel := ForwardingInfo{NextHop: NewChannelNextHop(scid)}
	require.False(t, channel.IsExit())
	require.Equal(t, fn.Some(scid), channel.NextHopChannel())

	// A node-ID next hop is always a forward and never exposes an outgoing
	// channel, since the switch selects one via non-strict forwarding.
	node := ForwardingInfo{NextHop: NewNodeNextHop(nodeID)}
	require.False(t, node.IsExit())
	require.Equal(
		t, fn.None[lnwire.ShortChannelID](), node.NextHopChannel(),
	)
}
