package hop_test

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

var (
	//nolint:lll
	testPrivKeyBytes, _ = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	_, testPubKey       = btcec.PrivKeyFromBytes(testPrivKeyBytes)
)

const testUnknownRequiredType = 0x80

type decodePayloadTest struct {
	name               string
	payload            []byte
	expErr             error
	expCustomRecords   map[uint64][]byte
	shouldHaveMPP      bool
	shouldHaveAMP      bool
	shouldHaveEncData  bool
	shouldHaveBlinding bool
	shouldHaveMetadata bool
	shouldHaveTotalAmt bool
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
		name: "required type after omitted hop id",
		payload: []byte{
			0x02, 0x00, 0x04, 0x00,
			testUnknownRequiredType, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      testUnknownRequiredType,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type after included hop id",
		payload: []byte{
			0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x01, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00,
			testUnknownRequiredType, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      testUnknownRequiredType,
			Violation: hop.RequiredViolation,
			FinalHop:  false,
		},
	},
	{
		name:    "required type zero final hop",
		payload: []byte{0x00, 0x00, 0x02, 0x00, 0x04, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type zero final hop zero sid",
		payload: []byte{0x00, 0x00, 0x02, 0x00, 0x04, 0x00, 0x06, 0x08,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  true,
		},
	},
	{
		name: "required type zero intermediate hop",
		payload: []byte{0x00, 0x00, 0x02, 0x00, 0x04, 0x00, 0x06, 0x08,
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  false,
		},
	},
	{
		name: "required type in custom range",
		payload: []byte{0x02, 0x00, 0x04, 0x00,
			0xfe, 0x00, 0x01, 0x00, 0x00, 0x02, 0x10, 0x11,
		},
		expCustomRecords: map[uint64][]byte{
			65536: {0x10, 0x11},
		},
	},
	{
		name: "valid intermediate hop",
		payload: []byte{0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x01, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
		expErr: nil,
	},
	{
		name:    "valid final hop",
		payload: []byte{0x02, 0x00, 0x04, 0x00},
		expErr:  nil,
	},
	{
		name: "intermediate hop with mpp",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// next hop id
			0x06, 0x08,
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			// mpp
			0x08, 0x21,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x08,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.MPPOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  false,
		},
	},
	{
		name: "intermediate hop with amp",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// next hop id
			0x06, 0x08,
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			// amp
			0x0e, 0x41,
			// amp.root_share
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			// amp.set_id
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			// amp.child_index
			0x09,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.AMPOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  false,
		},
	},
	{
		name: "intermediate hop with encrypted data",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		shouldHaveEncData: true,
	},
	{
		name: "intermediate hop with blinding point",
		payload: append([]byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// blinding point (type / length)
			0x0c, 0x21,
		},
			// blinding point (value)
			testPubKey.SerializeCompressed()...,
		),
		shouldHaveBlinding: true,
	},
	{
		name: "final hop with mpp",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// mpp
			0x08, 0x21,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x08,
		},
		expErr:        nil,
		shouldHaveMPP: true,
	},
	{
		name: "final hop with amp",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// amp
			0x0e, 0x41,
			// amp.root_share
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			// amp.set_id
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			// amp.child_index
			0x09,
		},
		shouldHaveAMP: true,
	},
	{
		name: "final hop with metadata",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// metadata
			0x10, 0x03, 0x01, 0x02, 0x03,
		},
		shouldHaveMetadata: true,
	},
	{
		name: "final hop with total amount",
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// total amount
			0x12, 0x01, 0x01,
		},
		shouldHaveTotalAmt: true,
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
	var (
		testTotalMsat = lnwire.MilliSatoshi(8)
		testAddr      = [32]byte{
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		}

		testRootShare = [32]byte{
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
			0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12, 0x12,
		}
		testSetID = [32]byte{
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
			0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13, 0x13,
		}
		testEncData    = []byte{3, 2, 1}
		testMetadata   = []byte{1, 2, 3}
		testChildIndex = uint32(9)
	)

	p, err := hop.NewPayloadFromReader(bytes.NewReader(test.payload))
	if !reflect.DeepEqual(test.expErr, err) {
		t.Fatalf("expected error mismatch, want: %v, got: %v",
			test.expErr, err)
	}
	if err != nil {
		return
	}

	// Assert MPP fields if we expect them.
	if test.shouldHaveMPP {
		if p.MPP == nil {
			t.Fatalf("payload should have MPP record")
		}
		if p.MPP.TotalMsat() != testTotalMsat {
			t.Fatalf("invalid total msat")
		}
		if p.MPP.PaymentAddr() != testAddr {
			t.Fatalf("invalid payment addr")
		}
	} else if p.MPP != nil {
		t.Fatalf("unexpected MPP payload")
	}

	if test.shouldHaveAMP {
		if p.AMP == nil {
			t.Fatalf("payload should have AMP record")
		}
		require.Equal(t, testRootShare, p.AMP.RootShare())
		require.Equal(t, testSetID, p.AMP.SetID())
		require.Equal(t, testChildIndex, p.AMP.ChildIndex())
	} else if p.AMP != nil {
		t.Fatalf("unexpected AMP payload")
	}

	if test.shouldHaveMetadata {
		if p.Metadata() == nil {
			t.Fatalf("payload should have metadata")
		}
		require.Equal(t, testMetadata, p.Metadata())
	} else if p.Metadata() != nil {
		t.Fatalf("unexpected metadata")
	}

	if test.shouldHaveEncData {
		require.NotNil(t, p.EncryptedData(),
			"payment should have encrypted data")

		require.Equal(t, testEncData, p.EncryptedData())
	} else {
		require.Nil(t, p.EncryptedData())
	}

	if test.shouldHaveBlinding {
		require.NotNil(t, p.BlindingPoint())

		require.Equal(t, testPubKey, p.BlindingPoint())
	} else {
		require.Nil(t, p.BlindingPoint())
	}

	if test.shouldHaveTotalAmt {
		require.NotZero(t, p.TotalAmtMsat())
	} else {
		require.Zero(t, p.TotalAmtMsat())
	}

	// Convert expected nil map to empty map, because we always expect an
	// initiated map from the payload.
	expCustomRecords := make(record.CustomSet)
	if test.expCustomRecords != nil {
		expCustomRecords = test.expCustomRecords
	}
	if !reflect.DeepEqual(expCustomRecords, p.CustomRecords()) {
		t.Fatalf("invalid custom records")
	}
}
