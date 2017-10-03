package htlcswitch

import (
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
)

// TestPaymentCircuitMapStorage test the decode/encode abitlities of the
// payment circuit and also check that the logic of circuit storage is right.
func TestPaymentCircuitMapStorage(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	circuit1 := &paymentCircuit{
		PaymentHash: [32]byte{1},
		Src:         lnwire.NewShortChanIDFromInt(1),
		Dest:        lnwire.NewShortChanIDFromInt(2),
		Obfuscator:  newMockObfuscator(),
	}

	circuitMap := newCircuitMap(cdb)

	if err := circuitMap.add(circuit1); err != nil {
		t.Fatalf("unable to add payment circuit: %v", err)
	}

	if circuitMap.pending() != 1 {
		t.Fatal("wrong number of payment circuits")
	}

	if err := circuitMap.add(circuit1); err != nil {
		t.Fatalf("unable to add payment circuit: %v", err)
	}

	if circuitMap.pending() != 2 {
		t.Fatal("wrong number of payment circuits")
	}

	circuit2, err := circuitMap.remove(circuit1.Key())
	if err != nil {
		t.Fatalf("unable to remove payment circuit: %v", err)
	}

	if circuitMap.pending() != 1 {
		t.Fatal("wrong number of payment circuits")
	}

	if !reflect.DeepEqual(circuit1, circuit2) {
		t.Fatal("payment circuits serialization is wrong")
	}
}
