package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestInitEncodeDecode(t *testing.T) {
	const somefeature = "somefeature"

	gf := NewFeatureVector([]Feature{
		{somefeature, OptionalFlag},
	})
	lf := NewFeatureVector([]Feature{
		{somefeature, OptionalFlag},
	})

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
	// init messages will differ. Set feature map with nil in
	// order to use deep equal function.
	init1.GlobalFeatures.featuresMap = nil
	init1.LocalFeatures.featuresMap = nil

	// Assert equality of the two instances.
	if !reflect.DeepEqual(init1, init2) {
		t.Fatalf("encode/decode init messages don't match %#v vs %#v",
			init1, init2)
	}
}
