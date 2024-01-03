package route

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

var (
	testPrivKeyBytes, _ = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	_, testPubKey       = btcec.PrivKeyFromBytes(testPrivKeyBytes)
	testPubKeyBytes, _  = NewVertexFromBytes(testPubKey.SerializeCompressed())
)

// TestRouteTotalFees checks that a route reports the expected total fee.
func TestRouteTotalFees(t *testing.T) {
	t.Parallel()

	// Make sure empty route returns a 0 fee, and zero amount.
	r := &Route{}
	if r.TotalFees() != 0 {
		t.Fatalf("expected 0 fees, got %v", r.TotalFees())
	}
	if r.ReceiverAmt() != 0 {
		t.Fatalf("expected 0 amt, got %v", r.ReceiverAmt())
	}

	// Make sure empty route won't be allowed in the constructor.
	amt := lnwire.MilliSatoshi(1000)
	_, err := NewRouteFromHops(amt, 100, Vertex{}, []*Hop{})
	if err != ErrNoRouteHopsProvided {
		t.Fatalf("expected ErrNoRouteHopsProvided, got %v", err)
	}

	// For one-hop routes the fee should be 0, since the last node will
	// receive the full amount.
	hops := []*Hop{
		{
			PubKeyBytes:      Vertex{},
			ChannelID:        1,
			OutgoingTimeLock: 44,
			AmtToForward:     amt,
		},
	}
	r, err = NewRouteFromHops(amt, 100, Vertex{}, hops)
	if err != nil {
		t.Fatal(err)
	}

	if r.TotalFees() != 0 {
		t.Fatalf("expected 0 fees, got %v", r.TotalFees())
	}

	if r.ReceiverAmt() != amt {
		t.Fatalf("expected %v amt, got %v", amt, r.ReceiverAmt())
	}

	// Append the route with a node, making the first one take a fee.
	fee := lnwire.MilliSatoshi(100)
	hops = append(hops, &Hop{
		PubKeyBytes:      Vertex{},
		ChannelID:        2,
		OutgoingTimeLock: 33,
		AmtToForward:     amt - fee,
	},
	)

	r, err = NewRouteFromHops(amt, 100, Vertex{}, hops)
	if err != nil {
		t.Fatal(err)
	}

	if r.TotalFees() != fee {
		t.Fatalf("expected %v fees, got %v", fee, r.TotalFees())
	}

	if r.ReceiverAmt() != amt-fee {
		t.Fatalf("expected %v amt, got %v", amt-fee, r.ReceiverAmt())
	}
}

var (
	testAmt  = lnwire.MilliSatoshi(1000)
	testAddr = [32]byte{0x01, 0x02}
)

// TestMPPHop asserts that a Hop will encode a non-nil MPP to final nodes, and
// fail when trying to send to intermediaries.
func TestMPPHop(t *testing.T) {
	t.Parallel()

	hop := Hop{
		ChannelID:        1,
		OutgoingTimeLock: 44,
		AmtToForward:     testAmt,
		LegacyPayload:    false,
		MPP:              record.NewMPP(testAmt, testAddr),
	}

	// Encoding an MPP record to an intermediate hop should result in a
	// failure.
	var b bytes.Buffer
	err := hop.PackHopPayload(&b, 2, false)
	if err != ErrIntermediateMPPHop {
		t.Fatalf("expected err: %v, got: %v",
			ErrIntermediateMPPHop, err)
	}

	// Encoding an MPP record to a final hop should be successful.
	b.Reset()
	err = hop.PackHopPayload(&b, 0, true)
	if err != nil {
		t.Fatalf("expected err: %v, got: %v", nil, err)
	}
}

// TestAMPHop asserts that a Hop will encode a non-nil AMP to final nodes of an
// MPP record is also present, and fail otherwise.
func TestAMPHop(t *testing.T) {
	t.Parallel()

	hop := Hop{
		ChannelID:        1,
		OutgoingTimeLock: 44,
		AmtToForward:     testAmt,
		LegacyPayload:    false,
		AMP:              record.NewAMP([32]byte{}, [32]byte{}, 3),
	}

	// Encoding an AMP record to an intermediate hop w/o an MPP record
	// should result in a failure.
	var b bytes.Buffer
	err := hop.PackHopPayload(&b, 2, false)
	if err != ErrAMPMissingMPP {
		t.Fatalf("expected err: %v, got: %v",
			ErrAMPMissingMPP, err)
	}

	// Encoding an AMP record to a final hop w/o an MPP record should result
	// in a failure.
	b.Reset()
	err = hop.PackHopPayload(&b, 0, true)
	if err != ErrAMPMissingMPP {
		t.Fatalf("expected err: %v, got: %v",
			ErrAMPMissingMPP, err)
	}

	// Encoding an AMP record to a final hop w/ an MPP record should be
	// successful.
	hop.MPP = record.NewMPP(testAmt, testAddr)
	b.Reset()
	err = hop.PackHopPayload(&b, 0, true)
	if err != nil {
		t.Fatalf("expected err: %v, got: %v", nil, err)
	}
}

// TestBlindedHops tests packing of a hop payload for various types of hops in
// a blinded route.
func TestBlindedHops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hop         Hop
		nextChannel uint64
		isFinal     bool
		err         error
	}{
		{
			name: "introduction point with next channel",
			hop: Hop{
				EncryptedData: []byte{1, 2, 3},
				BlindingPoint: testPubKey,
			},
			nextChannel: 1,
			isFinal:     false,
			err:         ErrUnexpectedField,
		},
		{
			name: "final node with next channel",
			hop: Hop{
				EncryptedData:    []byte{1, 2, 3},
				AmtToForward:     150,
				OutgoingTimeLock: 26,
			},
			nextChannel: 1,
			isFinal:     true,
			err:         ErrUnexpectedField,
		},
		{
			name: "valid introduction point",
			hop: Hop{
				EncryptedData: []byte{1, 2, 3},
				BlindingPoint: testPubKey,
			},
			nextChannel: 0,
			isFinal:     false,
		},
		{
			name: "valid intermediate blinding",
			hop: Hop{
				EncryptedData: []byte{1, 2, 3},
			},
			nextChannel: 0,
			isFinal:     false,
		},
		{
			name: "final blinded missing amount",
			hop: Hop{
				EncryptedData: []byte{1, 2, 3},
			},
			nextChannel: 0,
			isFinal:     true,
			err:         ErrMissingField,
		},
		{
			name: "final blinded expiry missing",
			hop: Hop{
				EncryptedData: []byte{1, 2, 3},
				AmtToForward:  100,
			},
			nextChannel: 0,
			isFinal:     true,
			err:         ErrMissingField,
		},
		{
			name: "valid final blinded",
			hop: Hop{
				EncryptedData:    []byte{1, 2, 3},
				AmtToForward:     100,
				OutgoingTimeLock: 52,
			},
			nextChannel: 0,
			isFinal:     true,
		},
		{
			// The introduction node can also be the final hop.
			name: "valid final intro blinded",
			hop: Hop{
				EncryptedData:    []byte{1, 2, 3},
				BlindingPoint:    testPubKey,
				AmtToForward:     100,
				OutgoingTimeLock: 52,
			},
			nextChannel: 0,
			isFinal:     true,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			err := testCase.hop.PackHopPayload(
				&b, testCase.nextChannel, testCase.isFinal,
			)
			require.ErrorIs(t, err, testCase.err)
		})
	}
}

// TestPayloadSize tests the payload size calculation that is provided by Hop
// structs.
func TestPayloadSize(t *testing.T) {
	t.Parallel()

	hops := []*Hop{
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     2000,
			OutgoingTimeLock: 600000,
			ChannelID:        3432483437438,
			LegacyPayload:    true,
		},
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1500,
			OutgoingTimeLock: 700000,
			ChannelID:        63584534844,
		},
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1000,
			OutgoingTimeLock: 700000,
			ChannelID:        51784534844,
			MPP:              record.NewMPP(500, [32]byte{}),
			AMP: record.NewAMP(
				[32]byte{}, [32]byte{}, 8,
			),
			CustomRecords: map[uint64][]byte{
				100000:  {1, 2, 3},
				1000000: {4, 5},
			},
			Metadata: []byte{10, 11},
		},
	}

	blindedHops := []*Hop{
		{
			// Unblinded hop to introduction node.
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1000,
			OutgoingTimeLock: 600000,
			ChannelID:        3432483437438,
			LegacyPayload:    true,
		},
		{
			// Payload for an introduction node in a blinded route
			// that has the blinding point provided in the onion
			// payload, and encrypted data pointing it to the next
			// node.
			PubKeyBytes:   testPubKeyBytes,
			EncryptedData: []byte{12, 13},
			BlindingPoint: testPubKey,
		},
		{
			// Payload for a forwarding node in a blinded route
			// that has encrypted data provided in the onion
			// payload, but no blinding point (it's provided in
			// update_add_htlc).
			PubKeyBytes:   testPubKeyBytes,
			EncryptedData: []byte{12, 13},
		},
		{
			// Final hop has encrypted data and other final hop
			// fields like metadata.
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     900,
			OutgoingTimeLock: 50000,
			Metadata:         []byte{10, 11},
			EncryptedData:    []byte{12, 13},
			TotalAmtMsat:     lnwire.MilliSatoshi(900),
		},
	}

	testCases := []struct {
		name string
		hops []*Hop
	}{
		{
			name: "clear route",
			hops: hops,
		},
		{
			name: "blinded route",
			hops: blindedHops,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testPayloadSize(t, testCase.hops)
		})
	}
}

func testPayloadSize(t *testing.T, hops []*Hop) {
	rt := Route{
		Hops: hops,
	}
	path, err := rt.ToSphinxPath()
	if err != nil {
		t.Fatal(err)
	}

	for i, onionHop := range path[:path.TrueRouteLength()] {
		hop := hops[i]
		var nextChan uint64
		if i < len(hops)-1 {
			nextChan = hops[i+1].ChannelID
		}

		expected := uint64(onionHop.HopPayload.NumBytes())
		actual := hop.PayloadSize(nextChan)
		if expected != actual {
			t.Fatalf("unexpected payload size at hop %v: "+
				"expected %v, got %v",
				i, expected, actual)
		}
	}
}

// TestBlindedHopFee tests calculation of hop fees for blinded routes.
func TestBlindedHopFee(t *testing.T) {
	t.Parallel()

	route := &Route{
		TotalAmount: 1500,
		Hops: []*Hop{
			// Start with two un-blinded hops.
			{
				AmtToForward: 1450,
			},
			{
				AmtToForward: 1300,
			},
			{
				// Our introduction node will have a zero
				// forward amount.
				AmtToForward: 0,
			},
			{
				// An intermediate blinded hop will also have
				// a zero forward amount.
				AmtToForward: 0,
			},
			{
				// Our final blinded hop should have a forward
				// amount set.
				AmtToForward: 1000,
			},
		},
	}

	require.Equal(t, lnwire.MilliSatoshi(50), route.HopFee(0))
	require.Equal(t, lnwire.MilliSatoshi(150), route.HopFee(1))
	require.Equal(t, lnwire.MilliSatoshi(300), route.HopFee(2))
	require.Equal(t, lnwire.MilliSatoshi(0), route.HopFee(3))
	require.Equal(t, lnwire.MilliSatoshi(0), route.HopFee(4))
}
