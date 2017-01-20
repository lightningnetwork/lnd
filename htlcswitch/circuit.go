package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/wire"
	"sync"
)

// CircuitKey uniquely identifies an active Sphinx (onion routing) circuit
// between two open channels. Currently, the rHash of the HTLC which created
// the circuit is used to uniquely identify each circuit.
type CircuitKey [sha256.Size]byte

func (k *CircuitKey) String() string {
	return hex.EncodeToString(k[:])
}

// PaymentCircuit is used by HTLC switch service in order to determine
// backward path for settle/cancel HTLC messages. A payment circuit is created
// once a htlc manager forwards an HTLC add request. Channel points contained
// within this message is used to identify the source/destination HTLC managers.
//
// NOTE: In current implementation of HTLC switch, the payment circuit might be
// uniquely identified by payment hash but in future we implement the payment
// fragmentation which makes possible for number of payments to have
// identical payments hashes, but different source and destinations.
//
// For example if Alice(A) want to send 2BTC to Bob(B), then payment will be
// split on two parts and node N3 will have circuit with the same payment hash,
// and destination, but different source (N1,N2).
//
//	    	  1BTC    N1   1BTC
//    	      + --------- o --------- +
//      2BTC  |	                      |  2BTC
// A o ------ o N0	           N3 o ------ o B
//	      |		              |
// 	      + --------- o --------- +
//	         1BTC     N2   1BTC
//
type PaymentCircuit struct {
	// PaymentHash used as uniq identifier of payment (not payment circuit)
	PaymentHash CircuitKey

	// Src is the channel id from which add HTLC request is came from and
	// to which settle/cancel HTLC request will be returned back. Once the
	// switch forwards the settle message to the source the circuit is
	// considered to be completed.
	Src *wire.OutPoint

	// Dest is the channel id to which we propagate the HTLC add request
	// and from which we are expecting to receive HTLC settle request back.
	Dest *wire.OutPoint
}

// NewPaymentCircuit creates new payment circuit instance.
func NewPaymentCircuit(src, dest *wire.OutPoint, key CircuitKey) *PaymentCircuit {
	return &PaymentCircuit{
		Src:         src,
		Dest:        dest,
		PaymentHash: key,
	}
}

// ID returns unique id of payment circuit.
func (a *PaymentCircuit) ID() string {
	return a.Src.String() +
		a.Dest.String() +
		a.PaymentHash.String()
}

// isEqual checks the equality of two payment circuits.
func (a *PaymentCircuit) IsEqual(b *PaymentCircuit) bool {
	return bytes.Equal(a.PaymentHash[:], b.PaymentHash[:]) &&
		a.Src.String() == b.Src.String() &&
		a.Dest.String() == b.Dest.String()
}

// circuitMap is a thread safe, persistent storage of circuits. Each
// circuit key (payment hash) might have numbers of circuits corresponding to it
// because of future payment fragmentation, now every circuit might be uniquely
// identified by payment hash (1-1 mapping).
type circuitMap struct {
	mutex    sync.RWMutex
	circuits map[CircuitKey][]*PaymentCircuit
}

// newCircuitMap initialized circuit map with previously stored circuits and
// return circuit map instance.
func newCircuitMap() (*circuitMap, error) {
	m := &circuitMap{
		circuits: make(map[CircuitKey][]*PaymentCircuit),
	}

	return m, nil
}

// add function add circuit in circuit map, and also save it in database in
// thread safe manner.
func (m *circuitMap) add(circuit *PaymentCircuit) error {
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
		m.circuits[circuit.PaymentHash] = make([]*PaymentCircuit, 1)
	}

	m.circuits[circuit.PaymentHash] = append(circuits, circuit)
	return nil
}

/// remove function removes circuit from map and database in thread safe manner.
func (m *circuitMap) remove(key CircuitKey, dest *wire.OutPoint) (
	*PaymentCircuit, error) {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	circuits, ok := m.circuits[key]
	if ok {
		for i, circuit := range circuits {
			if circuit.Dest.String() == dest.String() {
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
