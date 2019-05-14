package sphinx

import (
	"bytes"
	"testing"
)

func TestHopPayloadSizes(t *testing.T) {
	var tests = []struct {
		size     int
		expected int
		realm    byte
	}{
		{30, 1, 0x01},
		{32, 1, 0x01},
		{33, 2, 0x11},
		{97, 2, 0x11}, // The largest possible 2-hop payload
		{98, 3, 0x21},
		{162, 3, 0x21},
		{163, 4, 0x31},
	}

	for _, tt := range tests {
		hp, err := NewHopPayload(
			1, nil, bytes.Repeat([]byte{0x00}, tt.size),
		)
		if err != nil {
			t.Fatalf("unable to make hop payload: %v", err)
		}

		actual := hp.NumFrames()
		if actual != tt.expected {
			t.Errorf("wrong number of hops returned: expected "+
				"%d, actual %d", tt.expected, actual)
		}

		if hp.payloadRealm() != tt.realm {
			t.Errorf("payload realm did not match our "+
				"expectation: expected %q, actual %q", tt.realm,
				hp.Realm())
		}
	}
}
