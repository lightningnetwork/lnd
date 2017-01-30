package htlcswitch

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/wire"
	"sync"
)

// circuitMap is a thread safe, persistent storage of circuits. Each
// circuit key (payment hash) might have numbers of circuits corresponding to it
// because of future payment fragmentation, now every circuit might be uniquely
// identified by payment hash (1-1 mapping).
type circuitMap struct {
	mutex    sync.RWMutex
	circuits map[channeldb.CircuitKey][]*channeldb.PaymentCircuit
	db       *channeldb.DB
}

// newCircuitMap initialized circuit map with previously stored circuits and
// return circuit map instance.
func newCircuitMap(db *channeldb.DB) (*circuitMap, error) {
	m := &circuitMap{
		circuits: make(map[channeldb.CircuitKey][]*channeldb.PaymentCircuit),
	}

	circuits, err := db.FetchAllCircuits()
	if err != nil && err != channeldb.ErrNoCircuitsCreated {
		return nil, err
	}

	for _, circuit := range circuits {
		err := m.add(circuit)
		if err != nil {
			return nil, err
		}
	}

	// After synchronization circuit map with database, set db - thereby
	// we are turning on circuits persistence.
	m.db = db
	return m, nil
}

// add function add circuit in circuit map, and also save it in database in
// thread safe manner.
func (m *circuitMap) add(circuit *channeldb.PaymentCircuit) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	circuits, ok := m.circuits[circuit.PaymentHash]
	if ok {
		for _, c := range circuits {
			if circuit.IsEqual(c) {
				return errors.Errorf("Circuit for such "+
					"destination, source and payment hash "+
					"already exist %x", circuit)
			}
		}

	} else {
		m.circuits[circuit.PaymentHash] = make([]*channeldb.PaymentCircuit, 1)
	}

	// If database isn't empty we will sync circuit map with database, by
	// saving it.
	if err := m.db.AddCircuit(circuit); err != nil {
		return err
	}

	m.circuits[circuit.PaymentHash] = append(circuits, circuit)
	return nil
}

/// remove function removes circuit from map and database in thread safe manner.
func (m *circuitMap) remove(key channeldb.CircuitKey, dest *wire.OutPoint) (
	*channeldb.PaymentCircuit, error) {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	circuits, ok := m.circuits[key]
	if ok {
		for i, circuit := range circuits {
			if circuit.Dest.String() == dest.String() {
				err := m.db.RemoveCircuit(circuit)
				if err != nil {
					return nil, err
				}

				// Delete without preserving order
				// Google: Golang slice tricks
				circuits[i] = circuits[len(circuits)-1]
				circuits[len(circuits)-1] = nil
				m.circuits[key] = circuits[:len(circuits)-1]

				return circuit, nil
			}
		}
	}
	return nil, errors.Errorf("can't find circuit"+
		" for key %v and destination %v", key, dest.String())
}

// pending returns number of circuits which are waiting for to be completed
// (settle/cancel responses to be received)
func (m *circuitMap) pending() int {
	length := 0
	for _, circuits := range m.circuits {
		length += len(circuits)
	}

	return length
}
