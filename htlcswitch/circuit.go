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

// paymentCircuit is used by the htlc switch subsystem to determine the
// forwards/backwards path for the settle/fail HTLC messages. A payment circuit
// will be created once a channel link forwards the htlc add request and
// removed when we receive settle/fail htlc message.
type paymentCircuit struct {
	// PaymentHash used as unique identifier of payment.
	PaymentHash circuitKey

	// Src identifies the channel from which add htlc request is came from
	// and to which settle/fail htlc request will be returned back. Once
	// the switch forwards the settle/fail message to the src the circuit
	// is considered to be completed.
	Src lnwire.ShortChannelID

	// Dest identifies the channel to which we propagate the htlc add
	// update and from which we are expecting to receive htlc settle/fail
	// request back.
	Dest lnwire.ShortChannelID

	// ErrorEncrypter is used to re-encrypt the onion failure before
	// sending it back to the originator of the payment.
	ErrorEncrypter ErrorEncrypter

	// RefCount is used to count the circuits with the same circuit key.
	RefCount int
}

// newPaymentCircuit creates new payment circuit instance.
func newPaymentCircuit(src, dest lnwire.ShortChannelID, key circuitKey,
	e ErrorEncrypter) *paymentCircuit {

	return &paymentCircuit{
		Src:            src,
		Dest:           dest,
		PaymentHash:    key,
		RefCount:       1,
		ErrorEncrypter: e,
	}
}

// isEqual checks the equality of two payment circuits.
func (a *paymentCircuit) isEqual(b *paymentCircuit) bool {
	return bytes.Equal(a.PaymentHash[:], b.PaymentHash[:]) &&
		a.Src == b.Src &&
		a.Dest == b.Dest
}

// circuitMap is a data structure that implements thread safe storage of
// circuits. Each circuit key (payment hash) may have several of circuits
// corresponding to it due to the possibility of repeated payment hashes.
//
// TODO(andrew.shvv) make it persistent
type circuitMap struct {
	sync.RWMutex
	circuits map[circuitKey]*paymentCircuit
}

// newCircuitMap creates a new instance of the circuitMap.
func newCircuitMap() *circuitMap {
	return &circuitMap{
		circuits: make(map[circuitKey]*paymentCircuit),
	}
}

// add adds a new active payment circuit to the circuitMap.
func (m *circuitMap) add(circuit *paymentCircuit) error {
	m.Lock()
	defer m.Unlock()

	// Examine the circuit map to see if this circuit is already in use or
	// not. If so, then we'll simply increment the reference count.
	// Otherwise, we'll create a new circuit from scratch.
	//
	// TODO(roasbeef): include dest+src+amt in key
	if c, ok := m.circuits[circuit.PaymentHash]; ok {
		c.RefCount++
		return nil
	}

	m.circuits[circuit.PaymentHash] = circuit

	return nil
}

// remove destroys the target circuit by removing it from the circuit map.
func (m *circuitMap) remove(key circuitKey) (*paymentCircuit, error) {
	m.Lock()
	defer m.Unlock()

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
// (settle/fail responses to be received).
func (m *circuitMap) pending() int {
	m.RLock()
	defer m.RUnlock()

	var length int
	for _, circuits := range m.circuits {
		length += circuits.RefCount
	}

	return length
}
