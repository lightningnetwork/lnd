package hop_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/record"
)

type decodePayloadTest struct {
	name    string
	payload []byte
	expErr  error
}

var decodePayloadTests = []decodePayloadTest{
	{
		name:    "final hop valid",
		payload: []byte{0x02, 0x00, 0x04, 0x00},
	},
	{
		name: "intermediate hop valid",
		payload: []byte{0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x01, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
	},
	{
		name:    "final hop no amount",
		payload: []byte{0x04, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name: "intermediate hop no amount",
		payload: []byte{0x04, 0x00, 0x06, 0x08, 0x01, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  false,
		},
	},
	{
		name:    "final hop no expiry",
		payload: []byte{0x02, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name: "intermediate hop no expiry",
		payload: []byte{0x02, 0x00, 0x06, 0x08, 0x01, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  false,
		},
	},
	{
		name: "final hop next sid present",
		payload: []byte{0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  true,
		},
	},
	{
		name:    "required type after omitted hop id",
		payload: []byte{0x08, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      8,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type after included hop id",
		payload: []byte{0x06, 0x08, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x08, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      8,
			Violation: hop.RequiredViolation,
			FinalHop:  false,
		},
	},
	{
		name:    "required type zero final hop",
		payload: []byte{0x00, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type zero final hop zero sid",
		payload: []byte{0x00, 0x00, 0x06, 0x08, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type zero intermediate hop",
		payload: []byte{0x00, 0x00, 0x06, 0x08, 0x01, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  false,
		},
	},
}

// TestDecodeHopPayloadRecordValidation asserts that parsing the payloads in the
// tests yields the expected errors depending on whether the proper fields were
// included or omitted.
func TestDecodeHopPayloadRecordValidation(t *testing.T) {
	for _, test := range decodePayloadTests {
		t.Run(test.name, func(t *testing.T) {
			testDecodeHopPayloadValidation(t, test)
		})
	}
}

func testDecodeHopPayloadValidation(t *testing.T, test decodePayloadTest) {
	_, err := hop.NewPayloadFromReader(bytes.NewReader(test.payload))
	if !reflect.DeepEqual(test.expErr, err) {
		t.Fatalf("expected error mismatch, want: %v, got: %v",
			test.expErr, err)
	}
}
