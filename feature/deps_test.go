package feature

import (
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type depTest struct {
	name   string
	raw    *lnwire.RawFeatureVector
	expErr error
}

var depTests = []depTest{
	{
		name: "empty",
		raw:  lnwire.NewRawFeatureVector(),
	},
	{
		name: "no deps optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesOptional,
		),
	},
	{
		name: "no deps required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
		),
	},
	{
		name: "one dep optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrOptional,
		),
	},
	{
		name: "one dep required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrRequired,
		),
	},
	{
		name: "one missing optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrOptional,
		),
		expErr: ErrMissingFeatureDep{lnwire.TLVOnionPayloadOptional},
	},
	{
		name: "one missing required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrRequired,
		),
		expErr: ErrMissingFeatureDep{lnwire.TLVOnionPayloadOptional},
	},
	{
		name: "two dep optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
		),
	},
	{
		name: "two dep required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrRequired,
			lnwire.MPPRequired,
		),
	},
	{
		name: "two dep last missing optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
		),
		expErr: ErrMissingFeatureDep{lnwire.TLVOnionPayloadOptional},
	},
	{
		name: "two dep last missing required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrRequired,
			lnwire.MPPRequired,
		),
		expErr: ErrMissingFeatureDep{lnwire.TLVOnionPayloadOptional},
	},
	{
		name: "two dep first missing optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.MPPOptional,
		),
		expErr: ErrMissingFeatureDep{lnwire.PaymentAddrOptional},
	},
	{
		name: "two dep first missing required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.MPPRequired,
		),
		expErr: ErrMissingFeatureDep{lnwire.PaymentAddrOptional},
	},
	{
		name: "forest optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesOptional,
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
		),
	},
	{
		name: "forest required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesRequired,
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrRequired,
			lnwire.MPPRequired,
		),
	},
	{
		name: "broken forest optional",
		raw: lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesOptional,
			lnwire.TLVOnionPayloadRequired,
			lnwire.MPPOptional,
		),
		expErr: ErrMissingFeatureDep{lnwire.PaymentAddrOptional},
	},
	{
		name: "broken forest required",
		raw: lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesRequired,
			lnwire.TLVOnionPayloadRequired,
			lnwire.MPPRequired,
		),
		expErr: ErrMissingFeatureDep{lnwire.PaymentAddrOptional},
	},
}

// TestValidateDeps tests that ValidateDeps correctly asserts whether or not the
// set features constitute a valid feature chain when accounting for transititve
// dependencies.
func TestValidateDeps(t *testing.T) {
	for _, test := range depTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testValidateDeps(t, test)
		})
	}
}

func testValidateDeps(t *testing.T, test depTest) {
	fv := lnwire.NewFeatureVector(test.raw, lnwire.Features)
	err := ValidateDeps(fv)
	if !reflect.DeepEqual(err, test.expErr) {
		t.Fatalf("validation mismatch, want: %v, got: %v",
			test.expErr, err)
	}
}

// TestSettingDepBits sets that the SetBit function correctly sets a bit along
// with its dependencies in a feature vector. Specifically, we want to check
// that any existing optional bits are upgraded to required if the main bit
// being set is required. Similarly, if the main bit is optional, then any
// existing bits that depend on it should not be downgraded from required to
// optional.
func TestSettingDepBits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		existingVector *lnwire.RawFeatureVector
		newBit         lnwire.FeatureBit
		expectedVector *lnwire.RawFeatureVector
	}{
		{
			name:           "Optional bit with no dependants",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.ExplicitChannelTypeOptional,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.ExplicitChannelTypeOptional,
			),
		},
		{
			name:           "Required bit with no dependants",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.ExplicitChannelTypeRequired,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.ExplicitChannelTypeRequired,
			),
		},
		{
			name: "Optional bit with single " +
				"level dependant",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.RouteBlindingOptional,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.RouteBlindingOptional,
				lnwire.TLVOnionPayloadOptional,
			),
		},
		{
			name: "Required bit with single " +
				"level dependant",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.RouteBlindingRequired,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.RouteBlindingRequired,
				lnwire.TLVOnionPayloadRequired,
			),
		},
		{
			name: "Optional bit with multi level " +
				"dependants",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.Bolt11BlindedPathsOptional,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.Bolt11BlindedPathsOptional,
				lnwire.RouteBlindingOptional,
				lnwire.TLVOnionPayloadOptional,
			),
		},
		{
			name: "Required bit with multi level " +
				"dependants",
			existingVector: lnwire.NewRawFeatureVector(),
			newBit:         lnwire.Bolt11BlindedPathsRequired,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.Bolt11BlindedPathsRequired,
				lnwire.RouteBlindingRequired,
				lnwire.TLVOnionPayloadRequired,
			),
		},
		{
			name: "Existing required bit should not be " +
				"overridden if new bit is optional",
			existingVector: lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadRequired,
			),
			newBit: lnwire.Bolt11BlindedPathsOptional,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.Bolt11BlindedPathsOptional,
				lnwire.RouteBlindingOptional,
				lnwire.TLVOnionPayloadRequired,
			),
		},
		{
			name: "Existing optional bit should be overridden if " +
				"new bit is required",
			existingVector: lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
			),
			newBit: lnwire.Bolt11BlindedPathsRequired,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.Bolt11BlindedPathsRequired,
				lnwire.RouteBlindingRequired,
				lnwire.TLVOnionPayloadRequired,
			),
		},
		{
			name: "Unrelated bits should not be affected",
			existingVector: lnwire.NewRawFeatureVector(
				lnwire.AMPOptional,
				lnwire.TLVOnionPayloadOptional,
			),
			newBit: lnwire.Bolt11BlindedPathsRequired,
			expectedVector: lnwire.NewRawFeatureVector(
				lnwire.AMPOptional,
				lnwire.Bolt11BlindedPathsRequired,
				lnwire.RouteBlindingRequired,
				lnwire.TLVOnionPayloadRequired,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fv := lnwire.NewFeatureVector(
				test.existingVector, lnwire.Features,
			)

			resultFV := SetBit(fv, test.newBit)
			require.Equal(
				t, test.expectedVector,
				resultFV.RawFeatureVector,
			)
		})
	}
}

// TestSetBitNoCycles tests the SetBit call for each feature bit that we know of
// in both its optional and required form. This ensures that the SetBit call
// never gets stuck in a recursion cycle for any feature bit.
func TestSetBitNoCycles(t *testing.T) {
	t.Parallel()

	// For each feature-bit that we are aware of (both optional and
	// required), we will create a feature vector that is empty, and then
	// we will call SetBit with the given feature bit. We then check that
	// all the dependent features are also added in the appropriate form
	// (optional vs required). This test completing demonstrates that the
	// recursion in SetBit is not a problem since no feature bits should
	// create a dependency cycle.
	for bit := range lnwire.Features {
		fv := lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(), lnwire.Features,
		)

		resultFV := SetBit(fv, bit)

		// Ensure that all the dependent feature bits are in fact set
		// in the resulting set. Here we just check that some form
		// (optional or required) is set. The expected type is asserted
		// later on in the test.
		for expectedBit := range deps[bit] {
			require.True(t, resultFV.IsSet(expectedBit) ||
				resultFV.IsSet(mapToRequired(expectedBit)))
		}

		// Make sure all the resulting feature bits have the correct
		// form (optional vs required).
		for depBit := range resultFV.Features() {
			require.Equal(t, bit.IsRequired(), depBit.IsRequired())
		}
	}
}
