package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

// TestFeaturesRemoteRequireError checks that we throw an error if remote peer
// has required feature which we don't support.
func TestFeaturesRemoteRequireError(t *testing.T) {
	var (
		first featureName = "first"
		second featureName = "second"
	)

	var localFeaturesMap = FeaturesMap{
		first: 0,
	}

	var remoteFeaturesMap = FeaturesMap{
		first:  0,
		second: 1,
	}

	localFeatures := NewFeatureVector(localFeaturesMap)
	localFeatures.SetFeatureFlag(first, OptionalFlag)

	remoteFeatures := NewFeatureVector(remoteFeaturesMap)
	remoteFeatures.SetFeatureFlag(first, RequiredFlag)
	remoteFeatures.SetFeatureFlag(second, RequiredFlag)

	if _, err := localFeatures.Compare(remoteFeatures); err == nil {
		t.Fatal("error wasn't received")
	}
}

// TestFeaturesLocalRequireError checks that we throw an error if local peer has
// required feature which remote peer don't support.
func TestFeaturesLocalRequireError(t *testing.T) {
	var (
		first featureName = "first"
		second featureName = "second"
	)

	var localFeaturesMap = FeaturesMap{
		first:  0,
		second: 1,
	}

	var remoteFeaturesMap = FeaturesMap{
		first: 0,
	}

	localFeatures := NewFeatureVector(localFeaturesMap)
	localFeatures.SetFeatureFlag(first, OptionalFlag)
	localFeatures.SetFeatureFlag(second, RequiredFlag)

	remoteFeatures := NewFeatureVector(remoteFeaturesMap)
	remoteFeatures.SetFeatureFlag(first, RequiredFlag)

	if _, err := localFeatures.Compare(remoteFeatures); err == nil {
		t.Fatal("error wasn't received")
	}
}

// TestOptionalFeature checks that if remote peer don't have the feature but
// on our side this feature is optional than we mark this feature as disabled.
func TestOptionalFeature(t *testing.T) {
	var first featureName = "first"

	var localFeaturesMap = FeaturesMap{
		first: 0,
	}

	localFeatures := NewFeatureVector(localFeaturesMap)
	localFeatures.SetFeatureFlag(first, OptionalFlag)

	remoteFeatures := NewFeatureVector(FeaturesMap{})

	shared, err := localFeatures.Compare(remoteFeatures)
	if err != nil {
		t.Fatalf("error while feature vector compare: %v", err)
	}

	if shared.IsActive(first) {
		t.Fatal("locally feature was set but remote peer notified us" +
			" that it don't have it")
	}
}

// TestDecodeEncodeFeaturesVector checks that feature vector might be
// successfully encoded and decoded.
func TestDecodeEncodeFeaturesVector(t *testing.T) {
	var first featureName = "first"

	var localFeaturesMap = FeaturesMap{
		first: 0,
	}

	f := NewFeatureVector(localFeaturesMap)
	f.SetFeatureFlag(first, OptionalFlag)

	var b bytes.Buffer
	if err := f.Encode(&b); err != nil {
		t.Fatalf("error while encoding feature vector: %v", err)
	}

	nf, err := NewFeatureVectorFromReader(&b)
	if err != nil {
		t.Fatalf("error while decoding feature vector: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(f.flags, nf.flags) {
		t.Fatalf("encode/decode feature vector don't match %v vs "+
			"%v", f.String(), nf.String())
	}
}
