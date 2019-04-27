package wtdb_test

import (
	"bytes"
	"io"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// dbObject is abstract object support encoding and decoding.
type dbObject interface {
	Encode(io.Writer) error
	Decode(io.Reader) error
}

// TestCodec serializes and deserializes wtdb objects in order to test that that
// the codec understands all of the required field types. The test also asserts
// that decoding an object into another results in an equivalent object.
func TestCodec(t *testing.T) {
	mainScenario := func(obj dbObject) bool {
		// Ensure encoding the object succeeds.
		var b bytes.Buffer
		err := obj.Encode(&b)
		if err != nil {
			t.Fatalf("unable to encode: %v", err)
			return false
		}

		var obj2 dbObject
		switch obj.(type) {
		case *wtdb.SessionInfo:
			obj2 = &wtdb.SessionInfo{}
		case *wtdb.SessionStateUpdate:
			obj2 = &wtdb.SessionStateUpdate{}
		default:
			t.Fatalf("unknown type: %T", obj)
			return false
		}

		// Ensure decoding the object succeeds.
		err = obj2.Decode(bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatalf("unable to decode: %v", err)
			return false
		}

		// Assert the original and decoded object match.
		if !reflect.DeepEqual(obj, obj2) {
			t.Fatalf("encode/decode mismatch, want: %v, "+
				"got: %v", obj, obj2)
			return false
		}

		return true
	}

	tests := []struct {
		name     string
		scenario interface{}
	}{
		{
			name: "SessionInfo",
			scenario: func(obj wtdb.SessionInfo) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "SessionStateUpdate",
			scenario: func(obj wtdb.SessionStateUpdate) bool {
				return mainScenario(&obj)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := quick.Check(test.scenario, nil); err != nil {
				t.Fatalf("fuzz checks for msg=%s failed: %v",
					test.name, err)
			}
		})
	}
}
