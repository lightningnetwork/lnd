package htlcswitch_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestCircuitMap(t *testing.T) {
	t.Parallel()

	var hash1, hash2, hash3 [32]byte
	hash1[0] = 1
	hash2[0] = 2
	hash3[0] = 3

	var (
		chan1 = lnwire.NewShortChanIDFromInt(1)
		chan2 = lnwire.NewShortChanIDFromInt(2)
	)

	circuitMap := htlcswitch.NewCircuitMap()

	circuit := circuitMap.LookupByHTLC(chan1, 0)
	if circuit != nil {
		t.Fatalf("LookupByHTLC returned a circuit before any were added: %v",
			circuit)
	}

	// Add multiple circuits with same destination channel but different HTLC
	// IDs and payment hashes.
	circuitMap.Add(&htlcswitch.PaymentCircuit{
		PaymentHash: hash1,
		SrcChanID:   chan2,
		SrcHTLCID:   1,
		DestChanID:  chan1,
		DestHTLCID:  0,
	})

	circuitMap.Add(&htlcswitch.PaymentCircuit{
		PaymentHash: hash2,
		SrcChanID:   chan2,
		SrcHTLCID:   2,
		DestChanID:  chan1,
		DestHTLCID:  1,
	})

	// Add another circuit with an already-used HTLC ID but different
	// destination channel.
	circuitMap.Add(&htlcswitch.PaymentCircuit{
		PaymentHash: hash3,
		SrcChanID:   chan1,
		SrcHTLCID:   2,
		DestChanID:  chan2,
		DestHTLCID:  0,
	})

	circuit = circuitMap.LookupByHTLC(chan1, 0)
	if circuit == nil {
		t.Fatal("LookupByHTLC failed to find circuit")
	}
	if circuit.PaymentHash != hash1 || circuit.SrcHTLCID != 1 {
		t.Fatalf("LookupByHTLC found unexpected circuit: %v", circuit)
	}

	circuit = circuitMap.LookupByHTLC(chan1, 1)
	if circuit == nil {
		t.Fatal("LookupByHTLC failed to find circuit")
	}
	if circuit.PaymentHash != hash2 || circuit.SrcHTLCID != 2 {
		t.Fatalf("LookupByHTLC found unexpected circuit: %v", circuit)
	}

	circuit = circuitMap.LookupByHTLC(chan2, 0)
	if circuit == nil {
		t.Fatal("LookupByHTLC failed to find circuit")
	}
	if circuit.PaymentHash != hash3 || circuit.SrcHTLCID != 2 {
		t.Fatalf("LookupByHTLC found unexpected circuit: %v", circuit)
	}

	// Even though a circuit was added with chan1, HTLC ID 2 as the source, the
	// lookup should go by destination channel, HTLC ID.
	circuit = circuitMap.LookupByHTLC(chan1, 2)
	if circuit != nil {
		t.Fatalf("LookupByHTLC returned a circuit without being added: %v",
			circuit)
	}

	// Add a circuit with a destination channel and payment hash that are
	// already added but a different HTLC ID.
	circuitMap.Add(&htlcswitch.PaymentCircuit{
		PaymentHash: hash1,
		SrcChanID:   chan2,
		SrcHTLCID:   3,
		DestChanID:  chan1,
		DestHTLCID:  3,
	})

	circuit = circuitMap.LookupByHTLC(chan1, 3)
	if circuit == nil {
		t.Fatal("LookupByHTLC failed to find circuit")
	}
	if circuit.PaymentHash != hash1 || circuit.SrcHTLCID != 3 {
		t.Fatalf("LookupByHTLC found unexpected circuit: %v", circuit)
	}

	// Check lookups by payment hash.
	circuits := circuitMap.LookupByPaymentHash(hash1)
	if len(circuits) != 2 {
		t.Fatalf("LookupByPaymentHash returned wrong number of circuits for "+
			"hash1: expected %d, got %d", 2, len(circuits))
	}

	circuits = circuitMap.LookupByPaymentHash(hash2)
	if len(circuits) != 1 {
		t.Fatalf("LookupByPaymentHash returned wrong number of circuits for "+
			"hash2: expected %d, got %d", 1, len(circuits))
	}

	// Test removing circuits and the subsequent lookups.
	err := circuitMap.Remove(chan1, 0)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	circuits = circuitMap.LookupByPaymentHash(hash1)
	if len(circuits) != 1 {
		t.Fatalf("LookupByPaymentHash returned wrong number of circuits for "+
			"hash1: expecected %d, got %d", 1, len(circuits))
	}
	if circuits[0].DestHTLCID != 3 {
		t.Fatalf("LookupByPaymentHash returned wrong circuit for hash1: %v",
			circuits[0])
	}

	// Removing already-removed circuit should return an error.
	err = circuitMap.Remove(chan1, 0)
	if err == nil {
		t.Fatal("Remove did not return expected not found error")
	}

	// Remove last remaining circuit with payment hash hash1.
	err = circuitMap.Remove(chan1, 3)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	circuits = circuitMap.LookupByPaymentHash(hash1)
	if len(circuits) != 0 {
		t.Fatalf("LookupByPaymentHash returned wrong number of circuits for "+
			"hash1: expecected %d, got %d", 0, len(circuits))
	}
}
