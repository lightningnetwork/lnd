package sources

import (
	"fmt"
	"image/color"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testPub1 = "02000000000000000000000000000000" +
		"0000000000000000000000000000000001"
	testPub2 = "03000000000000000000000000000000" +
		"0000000000000000000000000000000001"
	testChanPoint = "abc123abc123abc123abc123abc123" +
		"abc123abc123abc123abc123abc123abcd"
)

// TestUnmarshalFeatures verifies that lnrpc feature maps are correctly
// converted to lnwire.FeatureVector.
func TestUnmarshalFeatures(t *testing.T) {
	rpcFeatures := map[uint32]*lnrpc.Feature{
		0:  {Name: "data-loss-protect", IsRequired: true},
		5:  {Name: "upfront-shutdown-script", IsKnown: true},
		9:  {Name: "tlv-onion", IsKnown: true},
		15: {Name: "payment-addr", IsKnown: true},
	}

	fv := unmarshalFeatures(rpcFeatures)
	require.NotNil(t, fv)
	require.True(t, fv.HasFeature(lnwire.FeatureBit(0)))
	require.True(t, fv.HasFeature(lnwire.FeatureBit(5)))
	require.True(t, fv.HasFeature(lnwire.FeatureBit(9)))
	require.True(t, fv.HasFeature(lnwire.FeatureBit(15)))
	require.False(t, fv.HasFeature(lnwire.FeatureBit(99)))
}

// TestUnmarshalFeaturesEmpty verifies that an empty feature map produces an
// empty (but non-nil) feature vector.
func TestUnmarshalFeaturesEmpty(t *testing.T) {
	fv := unmarshalFeatures(nil)
	require.NotNil(t, fv)
	require.False(t, fv.HasFeature(lnwire.FeatureBit(0)))
}

// TestParseColor verifies hex color string parsing.
func TestParseColor(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    color.RGBA
		wantErr bool
	}{
		{
			name:  "with hash",
			input: "#3399ff",
			want:  color.RGBA{R: 0x33, G: 0x99, B: 0xff},
		},
		{
			name:  "without hash",
			input: "ff0000",
			want:  color.RGBA{R: 0xff, G: 0x00, B: 0x00},
		},
		{
			name:  "empty string",
			input: "",
			want:  color.RGBA{},
		},
		{
			name:    "invalid length",
			input:   "#fff",
			wantErr: true,
		},
		{
			name:    "invalid hex",
			input:   "zzzzzz",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseColor(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestMakeCachedPolicy verifies that lnrpc.RoutingPolicy is correctly
// converted to a CachedEdgePolicy.
func TestMakeCachedPolicy(t *testing.T) {
	toNode := route.Vertex{0xaa}
	rpcPolicy := &lnrpc.RoutingPolicy{
		TimeLockDelta:           40,
		MinHtlc:                 1000,
		MaxHtlcMsat:             1_000_000_000,
		FeeBaseMsat:             1000,
		FeeRateMilliMsat:        100,
		Disabled:                false,
		InboundFeeBaseMsat:      -10,
		InboundFeeRateMilliMsat: -5,
	}

	policy := makeCachedPolicy(42, rpcPolicy, toNode, true)

	require.Equal(t, uint64(42), policy.ChannelID)
	require.True(t, policy.IsNode1)
	require.False(t, policy.IsDisabled)
	require.True(t, policy.HasMaxHTLC)
	require.Equal(t, uint16(40), policy.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(1000), policy.MinHTLC)
	require.Equal(t, lnwire.MilliSatoshi(1_000_000_000), policy.MaxHTLC)
	require.Equal(t, lnwire.MilliSatoshi(1000), policy.FeeBaseMSat)
	require.Equal(t, lnwire.MilliSatoshi(100),
		policy.FeeProportionalMillionths)

	// Check ToNodePubKey callback.
	require.Equal(t, toNode, policy.ToNodePubKey())

	// Check inbound fee.
	policy.InboundFee.WhenSome(func(fee lnwire.Fee) {
		require.Equal(t, int32(-10), fee.BaseFee)
		require.Equal(t, int32(-5), fee.FeeRate)
	})
}

// TestMakeCachedPolicyNoInboundFee verifies that when inbound fees are zero,
// the InboundFee option is None.
func TestMakeCachedPolicyNoInboundFee(t *testing.T) {
	rpcPolicy := &lnrpc.RoutingPolicy{
		TimeLockDelta:           40,
		MaxHtlcMsat:             1000,
		InboundFeeBaseMsat:      0,
		InboundFeeRateMilliMsat: 0,
	}

	policy := makeCachedPolicy(1, rpcPolicy, route.Vertex{}, true)
	require.True(t, policy.InboundFee.IsNone())
}

// TestMakeCachedPolicyDisabled verifies that a disabled policy is correctly
// flagged.
func TestMakeCachedPolicyDisabled(t *testing.T) {
	rpcPolicy := &lnrpc.RoutingPolicy{
		Disabled: true,
	}

	policy := makeCachedPolicy(1, rpcPolicy, route.Vertex{}, false)
	require.True(t, policy.IsDisabled)
	require.False(t, policy.IsNode1)
	require.False(t, policy.HasMaxHTLC)
}

// TestUnmarshalChannelEdge verifies full channel edge unmarshalling including
// both policies.
func TestUnmarshalChannelEdge(t *testing.T) {
	edge := &lnrpc.ChannelEdge{
		ChannelId: 123,
		ChanPoint: testChanPoint + ":0",
		Node1Pub:  testPub1,
		Node2Pub:  testPub2,
		Capacity:  1_000_000,
		Node1Policy: &lnrpc.RoutingPolicy{
			TimeLockDelta:    40,
			MinHtlc:          1000,
			MaxHtlcMsat:      500_000_000,
			FeeBaseMsat:      1000,
			FeeRateMilliMsat: 1,
			LastUpdate:       1700000000,
		},
		Node2Policy: &lnrpc.RoutingPolicy{
			TimeLockDelta:    80,
			MinHtlc:          2000,
			MaxHtlcMsat:      900_000_000,
			FeeBaseMsat:      2000,
			FeeRateMilliMsat: 2,
			LastUpdate:       1700000001,
			Disabled:         true,
		},
	}

	info, p1, p2, err := unmarshalChannelEdge(edge)
	require.NoError(t, err)

	require.Equal(t, uint64(123), info.ChannelID)
	require.Equal(t, int64(1_000_000), int64(info.Capacity))

	// Policy 1.
	require.NotNil(t, p1)
	require.Equal(t, uint16(40), p1.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(1000), p1.FeeBaseMSat)

	// Policy 2.
	require.NotNil(t, p2)
	require.Equal(t, uint16(80), p2.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(2000), p2.FeeBaseMSat)

	// Verify channel flags for disabled policy.
	require.True(t, p2.ChannelFlags.IsDisabled())
}

// TestUnmarshalChannelEdgeNilPolicies verifies that nil policies are handled.
func TestUnmarshalChannelEdgeNilPolicies(t *testing.T) {
	edge := &lnrpc.ChannelEdge{
		ChannelId: 456,
		ChanPoint: testChanPoint + ":1",
		Node1Pub:  testPub1,
		Node2Pub:  testPub2,
		Capacity:  500_000,
	}

	info, p1, p2, err := unmarshalChannelEdge(edge)
	require.NoError(t, err)
	require.Equal(t, uint64(456), info.ChannelID)
	require.Nil(t, p1)
	require.Nil(t, p2)
}

// TestUnmarshalPolicy verifies the full ChannelEdgePolicy unmarshalling
// including message flags and channel flags.
func TestUnmarshalPolicy(t *testing.T) {
	rpc := &lnrpc.RoutingPolicy{
		TimeLockDelta:    144,
		MinHtlc:          1000,
		MaxHtlcMsat:      100_000_000,
		FeeBaseMsat:      1000,
		FeeRateMilliMsat: 1,
		Disabled:         true,
		LastUpdate:       1700000000,
	}

	// Node 2 policy (not node1).
	policy, err := unmarshalPolicy(
		42, rpc, false, testPub1,
	)
	require.NoError(t, err)

	require.Equal(t, uint64(42), policy.ChannelID)
	require.True(t, policy.ChannelFlags.IsDisabled())
	require.True(t, policy.MessageFlags.HasMaxHtlc())

	// Direction bit should be set for node2.
	require.Equal(t, lnwire.ChanUpdateDirection|lnwire.ChanUpdateDisabled,
		policy.ChannelFlags)
}

// TestIsNotFound verifies the gRPC not-found detection helper.
func TestIsNotFound(t *testing.T) {
	require.False(t, isNotFound(nil))
	require.False(t, isNotFound(fmt.Errorf("random error")))

	// gRPC NotFound status error.
	err := status.Error(codes.NotFound, "not found")
	require.True(t, isNotFound(err))

	// Non-NotFound gRPC error.
	err = status.Error(codes.Internal, "internal")
	require.False(t, isNotFound(err))
}
