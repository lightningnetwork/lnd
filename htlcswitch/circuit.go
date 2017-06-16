package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// circuitKey uniquely identifies an active circuit between two open channels.
// Currently, the payment hash is used to uniquely identify each circuit.
type circuitKey [sha256.Size]byte

// String returns the string representation of the circuitKey.
func (k *circuitKey) String() string {
	return hex.EncodeToString(k[:])
}

// paymentCircuit is used by htlc switch subsystem in order to determine
// backward path for settle/fail htlc messages. A payment circuit will be
// created once a channel link forwards the htlc add request and removed when we
// receive settle/fail htlc message.
//
// NOTE: In current implementation of htlc switch, the payment circuit might be
// uniquely identified by payment hash but in future we implement the payment
// fragmentation which makes possible for number of payments to have
// identical payments hashes, but different source or destination.
//
// For example if Alice(A) want to send 2BTC to Bob(B), then payment will be
// split on two parts and node N3 will have circuit with the same payment hash,
// and destination, but different channel source (N1,N2).
//
//	    	  1BTC    N1   1BTC
//    	      + --------- o --------- +
//      2BTC  |	                      |  2BTC
// A o ------ o N0	           N3 o ------ o B
//	      |		              |
// 	      + --------- o --------- +
//	         1BTC     N2   1BTC
//
type paymentCircuit struct {
	// PaymentHash used as unique identifier of payment.
	PaymentHash circuitKey

	// Src identifies the channel from which add htlc request is came from
	// and to which settle/fail htlc request will be returned back. Once the
	// switch forwards the settle/fail message to the src the circuit is
	// considered to be completed.
	// TODO(andrew.shvv) use short channel id instead.
	Src lnwire.ChannelID

	// Dest identifies the channel to which we propagate the htlc add
	// update and from which we are expecting to receive htlc settle/fail
	// request back.
	// TODO(andrew.shvv) use short channel id instead.
	Dest lnwire.ChannelID

	// RefCount is used to count the circuits with the same circuit key.
	RefCount int
}

// newPaymentCircuit creates new payment circuit instance.
func newPaymentCircuit(src, dest lnwire.ChannelID, key circuitKey) *paymentCircuit {
	return &paymentCircuit{
		Src:         src,
		Dest:        dest,
		PaymentHash: key,
		RefCount:    1,
	}
}

// isEqual checks the equality of two payment circuits.
func (a *paymentCircuit) isEqual(b *paymentCircuit) bool {
	return bytes.Equal(a.PaymentHash[:], b.PaymentHash[:]) &&
		a.Src == b.Src &&
		a.Dest == b.Dest
}

// circuitMap is a thread safe storage of circuits. Each circuit key (payment
// hash) might have numbers of circuits corresponding to it
// because of future payment fragmentation, now every circuit might be uniquely
// identified by payment hash (1-1 mapping).
//
// NOTE: Also we have the htlc debug mode and in this mode we have the same
// payment hash for all htlcs.
// TODO(andrew.shvv) make it persistent
type circuitMap struct {
	mutex    sync.RWMutex
	circuits map[circuitKey]*paymentCircuit
}

// newCircuitMap initialized circuit map with previously stored circuits and
// return circuit map instance.
func newCircuitMap() *circuitMap {
	return &circuitMap{
		circuits: make(map[circuitKey]*paymentCircuit),
	}
}

// add function adds circuit in circuit map.
func (m *circuitMap) add(circuit *paymentCircuit) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Examine the circuit map to see if this
	// circuit is already in use or not. If so,
	// then we'll simply increment the reference
	// count. Otherwise, we'll create a new circuit
	// from scratch.
	// TODO(roasbeef): include dest+src+amt in key
	if c, ok := m.circuits[circuit.PaymentHash]; ok {
		c.RefCount++
	} else {
		m.circuits[circuit.PaymentHash] = circuit
	}

	return nil
}

// remove function removes circuit from map.
func (m *circuitMap) remove(key circuitKey) (
	*paymentCircuit, error) {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	if circuit, ok := m.circuits[key]; ok {
		if circuit.RefCount--; circuit.RefCount == 0 {
			delete(m.circuits, key)
		}
		return circuit, nil
	}

	return nil, errors.Errorf("can't find circuit"+
		" for key %v", key)
}

// pending returns number of circuits which are waiting for to be completed
// (settle/fail responses to be received)
func (m *circuitMap) pending() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var length int
	for _, circuits := range m.circuits {
		length += circuits.RefCount
	}

	return length
}
