package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

// TestFeaturesRemoteRequireError checks that we throw an error if remote peer
// has required feature which we don't support.
func TestFeaturesRemoteRequireError(t *testing.T) {
	const (
		first  = "first"
		second = "second"
	)

	localFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
	})

	remoteFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
		{second, RequiredFlag},
	})

	if _, err := localFeatures.Compare(remoteFeatures); err == nil {
		t.Fatal("error wasn't received")
	}
}

// TestFeaturesLocalRequireError checks that we throw an error if local peer has
// required feature which remote peer don't support.
func TestFeaturesLocalRequireError(t *testing.T) {
	const (
		first  = "first"
		second = "second"
	)

	localFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
		{second, RequiredFlag},
	})

	remoteFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
	})

	if _, err := localFeatures.Compare(remoteFeatures); err == nil {
		t.Fatal("error wasn't received")
	}
}

// TestOptionalFeature checks that if remote peer don't have the feature but
// on our side this feature is optional than we mark this feature as disabled.
func TestOptionalFeature(t *testing.T) {
	const first = "first"

	localFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
	})

	remoteFeatures := NewFeatureVector([]Feature{})

	shared, err := localFeatures.Compare(remoteFeatures)
	if err != nil {
		t.Fatalf("error while feature vector compare: %v", err)
	}

	if shared.IsActive(first) {
		t.Fatal("locally feature was set but remote peer notified us" +
			" that it don't have it")
	}

	// A feature with a non-existent name shouldn't be active.
	if shared.IsActive("nothere") {
		t.Fatal("non-existent feature shouldn't be active")
	}
}

// TestSetRequireAfterInit checks that we can change the feature flag after
// initialization.
func TestSetRequireAfterInit(t *testing.T) {
	const first = "first"

	localFeatures := NewFeatureVector([]Feature{
		{first, OptionalFlag},
	})
	localFeatures.SetFeatureFlag(first, RequiredFlag)
	remoteFeatures := NewFeatureVector([]Feature{})

	_, err := localFeatures.Compare(remoteFeatures)
	if err == nil {
		t.Fatalf("feature was set as required but error wasn't "+
			"returned: %v", err)
	}
}

// TestDecodeEncodeFeaturesVector checks that feature vector might be
// successfully encoded and decoded.
func TestDecodeEncodeFeaturesVector(t *testing.T) {
	const first = "first"

	f := NewFeatureVector([]Feature{
		{first, OptionalFlag},
	})

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
			"%v", spew.Sdump(f), spew.Sdump(nf))
	}
}

func TestFeatureFlagString(t *testing.T) {
	if OptionalFlag.String() != "optional" {
		t.Fatalf("incorrect string, expected optional got %v",
			OptionalFlag.String())
	}

	if RequiredFlag.String() != "required" {
		t.Fatalf("incorrect string, expected required got %v",
			OptionalFlag.String())
	}

	fakeFlag := featureFlag(9)
	if fakeFlag.String() != "<unknown>" {
		t.Fatalf("incorrect string, expected <unknown> got %v",
			fakeFlag.String())
	}
}
