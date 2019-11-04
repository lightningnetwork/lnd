package route

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// TestRouteTotalFees checks that a route reports the expected total fee.
func TestRouteTotalFees(t *testing.T) {
	t.Parallel()

	// Make sure empty route returns a 0 fee.
	r := &Route{}
	if r.TotalFees() != 0 {
		t.Fatalf("expected 0 fees, got %v", r.TotalFees())
	}

	// For one-hop routes the fee should be 0, since the last node will
	// receive the full amount.
	amt := lnwire.MilliSatoshi(1000)
	hops := []*Hop{
		{
			PubKeyBytes:      Vertex{},
			ChannelID:        1,
			OutgoingTimeLock: 44,
			AmtToForward:     amt,
		},
	}
	r, err := NewRouteFromHops(amt, 100, Vertex{}, hops)
	if err != nil {
		t.Fatal(err)
	}

	if r.TotalFees() != 0 {
		t.Fatalf("expected 0 fees, got %v", r.TotalFees())
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

}

var (
	testAmt  = lnwire.MilliSatoshi(1000)
	testAddr = [32]byte{0x01, 0x02}
)

// TestMPPHop asserts that a Hop will encode a non-nil to final nodes, and fail
// when trying to send to intermediaries.
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
