package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestInitEncodeDecode(t *testing.T) {
	fm := FeaturesMap{
		"somefeature": 0,
	}

	gf := NewFeatureVector(fm)
	gf.SetFeatureFlag("somefeature", OptionalFlag)

	lf := NewFeatureVector(fm)
	lf.SetFeatureFlag("somefeature", OptionalFlag)

	init1 := &Init{
		GlobalFeatures: gf,
		LocalFeatures:  lf,
	}

	// Next encode the init message into an empty bytes buffer.
	var b bytes.Buffer
	if err := init1.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode init: %v", err)
	}

	// Deserialize the encoded init message into a new empty struct.
	init2 := &Init{}
	if err := init2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode init: %v", err)
	}

	// We not encode the feature map in feature vector, for that reason the
	// init messages will differ. Initialize decoded feature map in
	// order to use deep equal function.
	init2.GlobalFeatures.features = fm
	init2.LocalFeatures.features = fm

	// Assert equality of the two instances.
	if !reflect.DeepEqual(init1, init2) {
		t.Fatalf("encode/decode init messages don't match %#v vs %#v",
			init1, init2)
	}
}
