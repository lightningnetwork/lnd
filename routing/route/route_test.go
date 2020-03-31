package route

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

var (
	testPrivKeyBytes, _ = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	_, testPubKey       = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKeyBytes)
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
	err := hop.PackHopPayload(&b, 2)
	if err != ErrIntermediateMPPHop {
		t.Fatalf("expected err: %v, got: %v",
			ErrIntermediateMPPHop, err)
	}

	// Encoding an MPP record to a final hop should be successful.
	b.Reset()
	err = hop.PackHopPayload(&b, 0)
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
	err := hop.PackHopPayload(&b, 2)
	if err != ErrAMPMissingMPP {
		t.Fatalf("expected err: %v, got: %v",
			ErrAMPMissingMPP, err)
	}

	// Encoding an AMP record to a final hop w/o an MPP record should result
	// in a failure.
	b.Reset()
	err = hop.PackHopPayload(&b, 0)
	if err != ErrAMPMissingMPP {
		t.Fatalf("expected err: %v, got: %v",
			ErrAMPMissingMPP, err)
	}

	// Encoding an AMP record to a final hop w/ an MPP record should be
	// successful.
	hop.MPP = record.NewMPP(testAmt, testAddr)
	b.Reset()
	err = hop.PackHopPayload(&b, 0)
	if err != nil {
		t.Fatalf("expected err: %v, got: %v", nil, err)
	}
}

// TestPayloadSize tests the payload size calculation that is provided by Hop
// structs.
func TestPayloadSize(t *testing.T) {
	hops := []*Hop{
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1000,
			OutgoingTimeLock: 600000,
			ChannelID:        3432483437438,
			LegacyPayload:    true,
		},
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1200,
			OutgoingTimeLock: 700000,
			ChannelID:        63584534844,
		},
		{
			PubKeyBytes:      testPubKeyBytes,
			AmtToForward:     1200,
			OutgoingTimeLock: 700000,
			MPP:              record.NewMPP(500, [32]byte{}),
			AMP:              record.NewAMP([32]byte{}, [32]byte{}, 8),
			CustomRecords: map[uint64][]byte{
				100000:  {1, 2, 3},
				1000000: {4, 5},
			},
		},
	}

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
