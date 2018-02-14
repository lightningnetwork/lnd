package htlcswitch_test

import (
	"bytes"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcutil"
)

var (
	hash1 = [32]byte{0x01}
	hash2 = [32]byte{0x02}
	hash3 = [32]byte{0x03}
)

func TestCircuitMapInit(t *testing.T) {
	t.Parallel()

	// Initialize new database for circuit map.
	cdb := makeCircuitDB(t, "")
	_, err := htlcswitch.NewCircuitMap(cdb)
	if err != nil {
		t.Fatalf("unable to create persistent circuit map: %v", err)
	}

	restartCircuitMap(t, cdb)
}

var halfCircuitTests = []struct {
	hash      [32]byte
	inValue   btcutil.Amount
	outValue  btcutil.Amount
	chanID    lnwire.ShortChannelID
	htlcID    uint64
	encrypter htlcswitch.ErrorEncrypter
}{
	{
		hash:      hash1,
		inValue:   0,
		outValue:  1000,
		chanID:    lnwire.NewShortChanIDFromInt(1),
		htlcID:    1,
		encrypter: nil,
	},
	{
		hash:      hash2,
		inValue:   2100,
		outValue:  2000,
		chanID:    lnwire.NewShortChanIDFromInt(2),
		htlcID:    2,
		encrypter: htlcswitch.NewMockObfuscator(),
	},
	{
		hash:      hash3,
		inValue:   10000,
		outValue:  9000,
		chanID:    lnwire.NewShortChanIDFromInt(3),
		htlcID:    3,
		encrypter: htlcswitch.NewSphinxErrorEncrypter(),
	},
}

// TestHalfCircuitSerialization checks that the half circuits can be properly
// encoded and decoded properly. A critical responsibility of this test is to
// verify that the various ErrorEncrypter implementations can be properly
// reconstructed from a serialized half circuit.
func TestHalfCircuitSerialization(t *testing.T) {
	for i, test := range halfCircuitTests {
		circuit := &htlcswitch.PaymentCircuit{
			PaymentHash:    test.hash,
			IncomingAmount: lnwire.NewMSatFromSatoshis(test.inValue),
			OutgoingAmount: lnwire.NewMSatFromSatoshis(test.outValue),
			Incoming: htlcswitch.CircuitKey{
				ChanID: test.chanID,
				HtlcID: test.htlcID,
			},
			ErrorEncrypter: test.encrypter,
		}

		// Write the half circuit to our buffer.
		var b bytes.Buffer
		if err := circuit.Encode(&b); err != nil {
			t.Fatalf("unable to encode half payment circuit test=%d: %v", i, err)
		}

		// Then try to decode the serialized bytes.
		var circuit2 htlcswitch.PaymentCircuit
		circuitReader := bytes.NewReader(b.Bytes())
		if err := circuit2.Decode(circuitReader); err != nil {
			t.Fatalf("unable to decode half payment circuit test=%d: %v", i, err)
		}

		// Reconstructed half circuit should match the original.
		if !reflect.DeepEqual(circuit, &circuit2) {
			t.Fatalf("unexpected half circuit test=%d, want %v, got %v",
				i, circuit, circuit2)
		}
	}
}

func TestCircuitMapPersistence(t *testing.T) {
	t.Parallel()

	var (
		chan1      = lnwire.NewShortChanIDFromInt(1)
		chan2      = lnwire.NewShortChanIDFromInt(2)
		circuitMap htlcswitch.CircuitMap
		err        error
	)

	cdb := makeCircuitDB(t, "")
	circuitMap, err = htlcswitch.NewCircuitMap(cdb)
	if err != nil {
		t.Fatalf("unable to create persistent circuit map: %v", err)
	}

	circuit := circuitMap.LookupCircuit(htlcswitch.CircuitKey{chan1, 0})
	if circuit != nil {
		t.Fatalf("LookupByHTLC returned a circuit before any were added: %v",
			circuit)
	}

	circuit1 := &htlcswitch.PaymentCircuit{
		Incoming: htlcswitch.CircuitKey{
			ChanID: chan2,
			HtlcID: 1,
		},
		PaymentHash:    hash1,
		ErrorEncrypter: htlcswitch.NewMockObfuscator(),
	}
	if _, err := circuitMap.CommitCircuits(circuit1); err != nil {
		t.Fatalf("unable to add half circuit: %v", err)
	}

	// Circuit map should have one circuit that has not been fully opened.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 0)
	assertHasCircuit(t, circuitMap, circuit1)

	cdb, circuitMap = restartCircuitMap(t, cdb)

	assertNumCircuitsWithHash(t, circuitMap, hash1, 0)
	assertHasCircuit(t, circuitMap, circuit1)

	// Add multiple circuits with same destination channel but different HTLC
	// IDs and payment hashes.
	keystone1 := htlcswitch.CircuitKey{
		ChanID: chan1,
		HtlcID: 0,
	}
	circuit1.Outgoing = &keystone1

	if err := circuitMap.SetKeystone(circuit1.Incoming, keystone1); err != nil {
		t.Fatalf("unable to add full circuit: %v", err)
	}

	// Circuit map should reflect addition of circuit1, and the change
	// should survive a restart.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)

	cdb, circuitMap = restartCircuitMap(t, cdb)

	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)

	circuit2 := &htlcswitch.PaymentCircuit{
		Incoming: htlcswitch.CircuitKey{
			ChanID: chan2,
			HtlcID: 2,
		},
		PaymentHash:    hash2,
		ErrorEncrypter: htlcswitch.NewMockObfuscator(),
	}
	if _, err := circuitMap.CommitCircuits(circuit2); err != nil {
		t.Fatalf("unable to add half circuit: %v", err)
	}

	assertHasCircuit(t, circuitMap, circuit2)

	keystone2 := htlcswitch.CircuitKey{
		ChanID: chan1,
		HtlcID: 1,
	}
	circuit2.Outgoing = &keystone2

	if err := circuitMap.SetKeystone(circuit2.Incoming, keystone2); err != nil {
		t.Fatalf("unable to add full circuit: %v", err)
	}

	// Should have two full circuits, one under hash1 and another under
	// hash2. Both half payment circuits should have been removed when the
	// full circuits were added.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)

	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertHasCircuit(t, circuitMap, circuit2)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)

	assertNumCircuitsWithHash(t, circuitMap, hash3, 0)

	cdb, circuitMap = restartCircuitMap(t, cdb)

	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)

	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertHasCircuit(t, circuitMap, circuit2)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)

	assertNumCircuitsWithHash(t, circuitMap, hash3, 0)

	circuit3 := &htlcswitch.PaymentCircuit{
		Incoming: htlcswitch.CircuitKey{
			ChanID: chan1,
			HtlcID: 2,
		},
		PaymentHash:    hash3,
		ErrorEncrypter: htlcswitch.NewMockObfuscator(),
	}
	if _, err := circuitMap.CommitCircuits(circuit3); err != nil {
		t.Fatalf("unable to add half circuit: %v", err)
	}

	assertHasCircuit(t, circuitMap, circuit3)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertHasCircuit(t, circuitMap, circuit3)

	// Add another circuit with an already-used HTLC ID but different
	// destination channel.
	keystone3 := htlcswitch.CircuitKey{
		ChanID: chan2,
		HtlcID: 0,
	}
	circuit3.Outgoing = &keystone3

	if err := circuitMap.SetKeystone(circuit3.Incoming, keystone3); err != nil {
		t.Fatalf("unable to add full circuit: %v", err)
	}

	// Check that all have been marked as full circuits, and that no half
	// circuits are currently being tracked.
	assertHasKeystone(t, circuitMap, keystone1, circuit1)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)
	assertHasKeystone(t, circuitMap, keystone3, circuit3)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)
	assertHasKeystone(t, circuitMap, keystone3, circuit3)

	// Even though a circuit was added with chan1, HTLC ID 2 as the source,
	// the lookup should go by destination channel, HTLC ID.
	invalidKeystone := htlcswitch.CircuitKey{
		ChanID: chan1,
		HtlcID: 2,
	}
	circuit = circuitMap.LookupByKeystone(invalidKeystone)
	if circuit != nil {
		t.Fatalf("LookupByHTLC returned a circuit without being added: %v",
			circuit)
	}

	circuit4 := &htlcswitch.PaymentCircuit{
		Incoming: htlcswitch.CircuitKey{
			ChanID: chan2,
			HtlcID: 3,
		},
		PaymentHash:    hash1,
		ErrorEncrypter: htlcswitch.NewMockObfuscator(),
	}
	if _, err := circuitMap.CommitCircuits(circuit4); err != nil {
		t.Fatalf("unable to add half circuit: %v", err)
	}

	// Circuit map should still only show one circuit with hash1, since we
	// have not set the keystone for circuit4.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit4)

	cdb, circuitMap = restartCircuitMap(t, cdb)

	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuit(t, circuitMap, circuit4)

	// Add a circuit with a destination channel and payment hash that are
	// already added but a different HTLC ID.
	keystone4 := htlcswitch.CircuitKey{
		ChanID: chan1,
		HtlcID: 3,
	}
	circuit4.Outgoing = &keystone4

	if err := circuitMap.SetKeystone(circuit4.Incoming, keystone4); err != nil {
		t.Fatalf("unable to add full circuit: %v", err)
	}

	// Verify that all circuits have been fully added.
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)
	assertHasCircuit(t, circuitMap, circuit2)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)
	assertHasCircuit(t, circuitMap, circuit3)
	assertHasKeystone(t, circuitMap, keystone3, circuit3)
	assertHasCircuit(t, circuitMap, circuit4)
	assertHasKeystone(t, circuitMap, keystone4, circuit4)

	// Verify that each circuit is exposed via the proper hash bucketing.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 2)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit1)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit4)

	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertHasCircuitForHash(t, circuitMap, hash2, circuit2)

	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)
	assertHasCircuitForHash(t, circuitMap, hash3, circuit3)

	// Restart, then run checks again.
	cdb, circuitMap = restartCircuitMap(t, cdb)

	// Verify that all circuits have been fully added.
	assertHasCircuit(t, circuitMap, circuit1)
	assertHasKeystone(t, circuitMap, keystone1, circuit1)
	assertHasCircuit(t, circuitMap, circuit2)
	assertHasKeystone(t, circuitMap, keystone2, circuit2)
	assertHasCircuit(t, circuitMap, circuit3)
	assertHasKeystone(t, circuitMap, keystone3, circuit3)
	assertHasCircuit(t, circuitMap, circuit4)
	assertHasKeystone(t, circuitMap, keystone4, circuit4)

	// Verify that each circuit is exposed via the proper hash bucketing.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 2)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit1)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit4)

	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertHasCircuitForHash(t, circuitMap, hash2, circuit2)

	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)
	assertHasCircuitForHash(t, circuitMap, hash3, circuit3)

	// Test removing circuits and the subsequent lookups.
	err = circuitMap.Delete(circuit1.Incoming)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	// There should be exactly one remaining circuit with hash1, and it
	// should be circuit4.
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit4)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit4)

	// Removing already-removed circuit should return an error.
	err = circuitMap.Delete(circuit1.Incoming)
	if err == nil {
		t.Fatal("Remove did not return expected not found error")
	}

	// Verify that nothing related to hash1 has changed
	assertNumCircuitsWithHash(t, circuitMap, hash1, 1)
	assertHasCircuitForHash(t, circuitMap, hash1, circuit4)

	// Remove last remaining circuit with payment hash hash1.
	err = circuitMap.Delete(circuit4.Incoming)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	assertNumCircuitsWithHash(t, circuitMap, hash1, 0)
	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertNumCircuitsWithHash(t, circuitMap, hash1, 0)
	assertNumCircuitsWithHash(t, circuitMap, hash2, 1)
	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)

	// Remove last remaining circuit with payment hash hash2.
	err = circuitMap.Delete(circuit2.Incoming)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	// There should now only be one remaining circuit, with hash3.
	assertNumCircuitsWithHash(t, circuitMap, hash2, 0)
	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertNumCircuitsWithHash(t, circuitMap, hash2, 0)
	assertNumCircuitsWithHash(t, circuitMap, hash3, 1)

	// Remove last remaining circuit with payment hash hash3.
	err = circuitMap.Delete(circuit3.Incoming)
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	// Check that the circuit map is empty, even after restarting.
	assertNumCircuitsWithHash(t, circuitMap, hash3, 0)
	cdb, circuitMap = restartCircuitMap(t, cdb)
	assertNumCircuitsWithHash(t, circuitMap, hash3, 0)
}

// assertHasKeystone tests that the circuit map contains the provided payment
// circuit.
func assertHasKeystone(t *testing.T, cm htlcswitch.CircuitMap,
	outKey htlcswitch.CircuitKey, c *htlcswitch.PaymentCircuit) {

	circuit := cm.LookupByKeystone(outKey)
	if !reflect.DeepEqual(circuit, c) {
		t.Fatalf("unexpected circuit, want: %v, got %v", c, circuit)
	}
}

// assertDoesNotHaveKeystone tests that the circuit map does not contain a
// circuit for the provided outgoing circuit key.
func assertDoesNotHaveKeystone(t *testing.T, cm htlcswitch.CircuitMap,
	outKey htlcswitch.CircuitKey) {

	circuit := cm.LookupByKeystone(outKey)
	if circuit != nil {
		t.Fatalf("expected no circuit for keystone %s, found %v",
			outKey, circuit)
	}
}

// assertHasCircuitForHash tests that the provided circuit appears in the list
// of circuits for the given hash.
func assertHasCircuitForHash(t *testing.T, cm htlcswitch.CircuitMap, hash [32]byte,
	circuit *htlcswitch.PaymentCircuit) {

	circuits := cm.LookupByPaymentHash(hash)
	for _, c := range circuits {
		if reflect.DeepEqual(c, circuit) {
			return
		}
	}

	t.Fatalf("unable to find circuit: %v by hash: %v", circuit, hash)
}

// assertNumCircuitsWithHash tests that the circuit has the right number of full
// circuits, indexed by the given hash.
func assertNumCircuitsWithHash(t *testing.T, cm htlcswitch.CircuitMap,
	hash [32]byte, expectedNum int) {

	circuits := cm.LookupByPaymentHash(hash)
	if len(circuits) != expectedNum {
		t.Fatalf("LookupByPaymentHash returned wrong number of circuits for "+
			"hash=%v: expecected %d, got %d", hash, expectedNum,
			len(circuits))
	}
}

// assertHasCircuit queries the circuit map using the half-circuit's half
// key, and fails if the returned half-circuit differs from the provided one.
func assertHasCircuit(t *testing.T, cm htlcswitch.CircuitMap,
	c *htlcswitch.PaymentCircuit) {

	c2 := cm.LookupCircuit(c.Incoming)
	if !reflect.DeepEqual(c, c2) {
		t.Fatalf("expected circuit: %v, got %v", c, c2)
	}
}

// assertDoesNotHaveCircuit queries the circuit map using the circuit's
// incoming circuit key, and fails if it is found.
func assertDoesNotHaveCircuit(t *testing.T, cm htlcswitch.CircuitMap,
	c *htlcswitch.PaymentCircuit) {

	c2 := cm.LookupCircuit(c.Incoming)
	if c2 != nil {
		t.Fatalf("expected no circuit for %v, got %v", c, c2)
	}
}

// makeCircuitDB initializes a new test channeldb for testing the persistence of
// the circuit map. If an empty string is provided as a path, a temp directory
// will be created.
func makeCircuitDB(t *testing.T, path string) *channeldb.DB {
	if path == "" {
		var err error
		path, err = ioutil.TempDir("", "circuitdb")
		if err != nil {
			t.Fatalf("unable to create temp path: %v", err)
		}
	}

	db, err := channeldb.Open(path)
	if err != nil {
		t.Fatalf("unable to open channel db: %v", err)
	}

	return db
}

// Creates a new circuit map, backed by a freshly opened channeldb. The existing
// channeldb is closed in order to simulate a complete restart.
func restartCircuitMap(t *testing.T, cdb *channeldb.DB) (*channeldb.DB,
	htlcswitch.CircuitMap) {

	// Record the current temp path and close current db.
	dbPath := cdb.Path()
	cdb.Close()

	// Reinitialize circuit map with same db path.
	cdb2 := makeCircuitDB(t, dbPath)
	cm2, err := htlcswitch.NewCircuitMap(cdb2)
	if err != nil {
		t.Fatalf("unable to recreate persistent circuit map: %v", err)
	}

	return cdb2, cm2
}
