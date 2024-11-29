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
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	//nolint:ll
	testPrivKeyBytes, _ = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	_, testPubKey       = btcec.PrivKeyFromBytes(testPrivKeyBytes)
)

const testUnknownRequiredType = 0x80

type decodePayloadTest struct {
	name               string
	payload            []byte
	isFinalHop         bool
	updateAddBlinded   bool
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
		name:       "final hop valid",
		isFinalHop: true,
		payload:    []byte{0x02, 0x00, 0x04, 0x00},
	},
	{
		name:       "intermediate hop valid",
		isFinalHop: false,
		payload: []byte{0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x01, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
	},
	{
		name:       "final hop no amount",
		payload:    []byte{0x04, 0x00},
		isFinalHop: true,
		expErr: hop.ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name:       "intermediate hop no amount",
		isFinalHop: false,
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
		name:       "final hop no expiry",
		isFinalHop: true,
		payload:    []byte{0x02, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name:       "intermediate hop no expiry",
		isFinalHop: false,
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
		name:       "final hop next sid present",
		isFinalHop: true,
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
		name:       "required type after omitted hop id",
		isFinalHop: true,
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
		name:       "required type after included hop id",
		isFinalHop: false,
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
		name:       "required type zero final hop",
		isFinalHop: true,
		payload:    []byte{0x00, 0x00, 0x02, 0x00, 0x04, 0x00},
		expErr: hop.ErrInvalidPayload{
			Type:      0,
			Violation: hop.RequiredViolation,
			FinalHop:  true,
		},
	},
	{
		name:       "required type zero final hop zero sid",
		isFinalHop: true,
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
		name:       "required type zero intermediate hop",
		isFinalHop: false,
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
		name:       "required type in custom range",
		isFinalHop: false,
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
			// next hop id
			0x06, 0x08,
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			// custom
			0xfe, 0x00, 0x01, 0x00, 0x00, 0x02, 0x10, 0x11,
		},
		expCustomRecords: map[uint64][]byte{
			65536: {0x10, 0x11},
		},
	},
	{
		name:       "valid intermediate hop",
		isFinalHop: false,
		payload: []byte{0x02, 0x00, 0x04, 0x00, 0x06, 0x08, 0x01, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
		expErr: nil,
	},
	{
		name:       "valid final hop",
		isFinalHop: true,
		payload:    []byte{0x02, 0x00, 0x04, 0x00},
		expErr:     nil,
	},
	{
		name:       "intermediate hop with mpp",
		isFinalHop: false,
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
		name:       "intermediate hop with amp",
		isFinalHop: false,
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
		name:       "intermediate hop no next channel",
		isFinalHop: false,
		payload: []byte{
			// amount
			0x02, 0x00,
			// cltv
			0x04, 0x00,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.NextHopOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  false,
		},
	},
	{
		name:             "intermediate hop with encrypted data",
		isFinalHop:       false,
		updateAddBlinded: true,
		payload: []byte{
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		shouldHaveEncData: true,
	},
	{
		name:       "intermediate hop with blinding point",
		isFinalHop: false,
		payload: append([]byte{
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
			// blinding point (type / length)
			0x0c, 0x21,
		},
			// blinding point (value)
			testPubKey.SerializeCompressed()...,
		),
		shouldHaveBlinding: true,
		shouldHaveEncData:  true,
	},
	{
		name:       "final hop with mpp",
		isFinalHop: true,
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
		name:       "final hop with amp",
		isFinalHop: true,
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
		name:       "final hop with metadata",
		isFinalHop: true,
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
		name:       "final hop with total amount",
		isFinalHop: true,
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
	{
		name:             "final blinded hop with total amount",
		isFinalHop:       true,
		updateAddBlinded: true,
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
		name:             "final blinded missing amt",
		isFinalHop:       true,
		updateAddBlinded: true,
		payload: []byte{
			// cltv
			0x04, 0x00,
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		shouldHaveEncData: true,
		expErr: hop.ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name:             "final blinded missing cltv",
		isFinalHop:       true,
		updateAddBlinded: true,
		payload: []byte{
			// amount
			0x02, 0x00,
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		shouldHaveEncData: true,
		expErr: hop.ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  true,
		},
	},
	{
		name:             "intermediate blinded has amount",
		isFinalHop:       false,
		updateAddBlinded: true,
		payload: []byte{
			// amount
			0x02, 0x00,
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.AmtOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  false,
		},
	},
	{
		name:             "intermediate blinded has expiry",
		isFinalHop:       false,
		updateAddBlinded: true,
		payload: []byte{
			// cltv
			0x04, 0x00,
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.LockTimeOnionType,
			Violation: hop.IncludedViolation,
			FinalHop:  false,
		},
	},
	{
		name:       "update add blinding no data",
		isFinalHop: false,
		payload: []byte{
			// cltv
			0x04, 0x00,
		},
		updateAddBlinded: true,
		expErr: hop.ErrInvalidPayload{
			Type:      record.EncryptedDataOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  false,
		},
	},
	{
		name:       "onion blinding point no data",
		isFinalHop: false,
		payload: append([]byte{
			// blinding point (type / length)
			0x0c, 0x21,
		},
			// blinding point (value)
			testPubKey.SerializeCompressed()...,
		),
		expErr: hop.ErrInvalidPayload{
			Type:      record.EncryptedDataOnionType,
			Violation: hop.OmittedViolation,
			FinalHop:  false,
		},
	},
	{
		name:       "encrypted data no blinding",
		isFinalHop: false,
		payload: []byte{
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
		},
		expErr: hop.ErrInvalidPayload{
			Type:      record.EncryptedDataOnionType,
			Violation: hop.IncludedViolation,
		},
	},
	{
		name:             "both blinding points",
		isFinalHop:       false,
		updateAddBlinded: true,
		payload: append([]byte{
			// encrypted data
			0x0a, 0x03, 0x03, 0x02, 0x01,
			// blinding point (type / length)
			0x0c, 0x21,
		},
			// blinding point (value)
			testPubKey.SerializeCompressed()...,
		),
		expErr: hop.ErrInvalidPayload{
			Type:      record.BlindingPointOnionType,
			Violation: hop.IncludedViolation,
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

	p, parsedTypes, err := hop.ParseTLVPayload(
		bytes.NewReader(test.payload),
	)
	require.NoError(t, err)

	err = hop.ValidateTLVPayload(
		parsedTypes, test.isFinalHop, test.updateAddBlinded,
	)
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

// TestValidateBlindedRouteData tests validation of the values provided in a
// blinded route.
func TestValidateBlindedRouteData(t *testing.T) {
	scid := lnwire.NewShortChanIDFromInt(1)

	tests := []struct {
		name             string
		data             *record.BlindedRouteData
		incomingAmount   lnwire.MilliSatoshi
		incomingTimelock uint32
		err              error
	}{
		{
			name: "max cltv expired",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{},
				&record.PaymentConstraints{
					MaxCltvExpiry: 100,
				},
				nil,
			),
			incomingTimelock: 200,
			err: hop.ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: hop.InsufficientViolation,
			},
		},
		{
			name: "zero max cltv",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{},
				&record.PaymentConstraints{
					MaxCltvExpiry:   0,
					HtlcMinimumMsat: 10,
				},
				nil,
			),
			incomingAmount:   100,
			incomingTimelock: 10,
			err: hop.ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: hop.InsufficientViolation,
			},
		},
		{
			name: "amount below minimum",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{},
				&record.PaymentConstraints{
					HtlcMinimumMsat: 15,
				},
				nil,
			),
			incomingAmount: 10,
			err: hop.ErrInvalidPayload{
				Type:      record.AmtOnionType,
				Violation: hop.InsufficientViolation,
			},
		},
		{
			name: "valid, no features",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{},
				&record.PaymentConstraints{
					MaxCltvExpiry:   100,
					HtlcMinimumMsat: 20,
				},
				nil,
			),
			incomingAmount:   40,
			incomingTimelock: 80,
		},
		{
			name: "unknown features",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{},
				&record.PaymentConstraints{
					MaxCltvExpiry:   100,
					HtlcMinimumMsat: 20,
				},
				lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(
						lnwire.FeatureBit(9999),
					),
					lnwire.Features,
				),
			),
			incomingAmount:   40,
			incomingTimelock: 80,
			err: hop.ErrInvalidPayload{
				Type:      14,
				Violation: hop.IncludedViolation,
			},
		},
		{
			name: "valid data",
			data: record.NewNonFinalBlindedRouteData(
				scid,
				nil,
				record.PaymentRelayInfo{
					CltvExpiryDelta: 10,
					FeeRate:         10,
					BaseFee:         100,
				},
				&record.PaymentConstraints{
					MaxCltvExpiry:   100,
					HtlcMinimumMsat: 20,
				},
				nil,
			),
			incomingAmount:   40,
			incomingTimelock: 80,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			err := hop.ValidateBlindedRouteData(
				testCase.data, testCase.incomingAmount,
				testCase.incomingTimelock,
			)
			require.Equal(t, testCase.err, err)
		})
	}
}

// TestValidatePayloadWithBlinded tests validation of the contents of a
// payload when it's for a blinded payment.
func TestValidatePayloadWithBlinded(t *testing.T) {
	t.Parallel()

	finalHopMap := map[tlv.Type][]byte{
		record.AmtOnionType:            nil,
		record.LockTimeOnionType:       nil,
		record.TotalAmtMsatBlindedType: nil,
	}

	tests := []struct {
		name    string
		isFinal bool
		parsed  map[tlv.Type][]byte
		err     bool
	}{
		{
			name:    "final hop, valid",
			isFinal: true,
			parsed:  finalHopMap,
		},
		{
			name:    "intermediate hop, invalid",
			isFinal: false,
			parsed:  finalHopMap,
			err:     true,
		},
		{
			name:    "intermediate hop, invalid",
			isFinal: false,
			parsed: map[tlv.Type][]byte{
				record.EncryptedDataOnionType: nil,
				record.BlindingPointOnionType: nil,
			},
		},
		{
			name:    "unknown record, invalid",
			isFinal: false,
			parsed: map[tlv.Type][]byte{
				tlv.Type(99): nil,
			},
			err: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := hop.ValidatePayloadWithBlinded(
				testCase.isFinal, testCase.parsed,
			)

			// We can't determine our exact error because we
			// iterate through a map (non-deterministic) in the
			// function.
			if testCase.err {
				require.NotNil(t, err)
			} else {
				require.Nil(t, err)
			}
		})
	}
}
