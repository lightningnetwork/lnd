package channeldb

import (
	"bytes"
	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var (
	hash1, _ = chainhash.NewHash(bytes.Repeat([]byte("a"), 32))
	hash2, _ = chainhash.NewHash(bytes.Repeat([]byte("b"), 32))

	chanPoint1 = wire.NewOutPoint(hash1, 0)
	chanPoint2 = wire.NewOutPoint(hash2, 0)
)

func randCircuit() (*PaymentCircuit, error) {
	rand.Seed(time.Now().UTC().UnixNano())

	var pre [32]byte
	if _, err := rand.Read(pre[:]); err != nil {
		return nil, err
	}

	c := &PaymentCircuit{
		Src:         chanPoint1,
		Dest:        chanPoint2,
		PaymentHash: pre,
	}

	return c, nil
}

// TestCircuitWorkflow check that adding, removing and fetching database
// circuits operations working properly.
func TestCircuitWorkflow(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// Create a fake circuit which we'll use several times in the tests
	// below.
	fakeCircuit, err := randCircuit()
	if err != nil {
		t.Fatalf("unable to create circuit: %v", err)
	}

	// Add the circuit to the database, this should succeed as there
	// aren't any existing circuit within the database with the same payment
	// hash.
	if err := db.AddCircuit(fakeCircuit); err != nil {
		t.Fatalf("unable to add circuit: %v", err)
	}

	// Attempt to insert generated above again, this should fail as
	// duplicates are rejected by the processing logic.
	if err := db.AddCircuit(fakeCircuit); err != ErrDuplicateCircuit {
		t.Fatalf("circuit insertion should fail due to duplication, "+
			"instead got %v", err)
	}

	// Attempt to fetch the circuits which was just added to the
	// database. It should be found, and the circuits returned should be
	// identical to the one created above.
	circuits, err := db.FetchAllCircuits()
	if err != nil {
		t.Fatalf("unable to find fetch circuits: %v", err)
	}

	if len(circuits) != 1 {
		t.Fatalf("wrong number of circuits expects 1 got %v", len(circuits))
	}

	if !reflect.DeepEqual(fakeCircuit, circuits[0]) {
		t.Fatalf("circuit fetched from db doesn't match original %v "+
			"vs %v", spew.Sdump(fakeCircuit), spew.Sdump(circuits[0]))
	}

	// Remove the circuits, the version retrieved from the database should
	// have zero circuits.
	if err := db.RemoveCircuit(circuits[0]); err != nil {
		t.Fatalf("unable to remove circuit: %v", err)
	}

	circuits, err = db.FetchAllCircuits()
	if err != nil {
		t.Fatalf("unable to find fetch circuits: %v", err)
	}

	if len(circuits) != 0 {
		t.Fatalf("wrong number of circuits expects 0 got %v", len(circuits))
	}
}
