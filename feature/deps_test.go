package feature

import (
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
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
			lnwire.TLVOnionPayloadOptional,
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
			lnwire.TLVOnionPayloadOptional,
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
			lnwire.TLVOnionPayloadOptional,
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
			lnwire.TLVOnionPayloadOptional,
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
			lnwire.TLVOnionPayloadOptional,
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
