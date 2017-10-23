package htlcswitch

import (
	"fmt"
	"sync"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// PaymentCircuit is used by the HTLC switch subsystem to determine the
// backwards path for the settle/fail HTLC messages. A payment circuit
// will be created once a channel link forwards the HTLC add request and
// removed when we receive a settle/fail HTLC message.
type PaymentCircuit struct {
	// PaymentHash used as unique identifier of payment.
	PaymentHash [32]byte

	// IncomingChanID identifies the channel from which add HTLC request came
	// and to which settle/fail HTLC request will be returned back. Once
	// the switch forwards the settle/fail message to the src the circuit
	// is considered to be completed.
	IncomingChanID lnwire.ShortChannelID

	// IncomingHTLCID is the ID in the update_add_htlc message we received from
	// the incoming channel, which will be included in any settle/fail messages
	// we send back.
	IncomingHTLCID uint64

	// OutgoingChanID identifies the channel to which we propagate the HTLC add
	// update and from which we are expecting to receive HTLC settle/fail
	// request back.
	OutgoingChanID lnwire.ShortChannelID

	// OutgoingHTLCID is the ID in the update_add_htlc message we sent to the
	// outgoing channel.
	OutgoingHTLCID uint64

	// ErrorEncrypter is used to re-encrypt the onion failure before
	// sending it back to the originator of the payment.
	ErrorEncrypter ErrorEncrypter
}

// circuitKey is a channel ID, HTLC ID tuple used as an identifying key for a
// payment circuit. The circuit map is keyed with the idenitifer for the
// outgoing HTLC
type circuitKey struct {
	chanID lnwire.ShortChannelID
	htlcID uint64
}

// String returns a string representation of the circuitKey.
func (k *circuitKey) String() string {
	return fmt.Sprintf("(Chan ID=%s, HTLC ID=%d)", k.chanID, k.htlcID)
}

// CircuitMap is a data structure that implements thread safe storage of
// circuit routing information. The switch consults a circuit map to determine
// where to forward HTLC update messages. Each circuit is stored with it's
// outgoing HTLC as the primary key because, each offered HTLC has at most one
// received HTLC, but there may be multiple offered or received HTLCs with the
// same payment hash. Circuits are also indexed to provide fast lookups by
// payment hash.
//
// TODO(andrew.shvv) make it persistent
type CircuitMap struct {
	mtx       sync.RWMutex
	circuits  map[circuitKey]*PaymentCircuit
	hashIndex map[[32]byte]map[PaymentCircuit]struct{}
}

// NewCircuitMap creates a new instance of the CircuitMap.
func NewCircuitMap() *CircuitMap {
	return &CircuitMap{
		circuits:  make(map[circuitKey]*PaymentCircuit),
		hashIndex: make(map[[32]byte]map[PaymentCircuit]struct{}),
	}
}

// LookupByHTLC looks up the payment circuit by the outgoing channel and HTLC
// IDs. Returns nil if there is no such circuit.
func (cm *CircuitMap) LookupByHTLC(chanID lnwire.ShortChannelID, htlcID uint64) *PaymentCircuit {
	cm.mtx.RLock()

	key := circuitKey{
		chanID: chanID,
		htlcID: htlcID,
	}
	circuit := cm.circuits[key]

	cm.mtx.RUnlock()
	return circuit
}

// LookupByPaymentHash looks up and returns any payment circuits with a given
// payment hash.
func (cm *CircuitMap) LookupByPaymentHash(hash [32]byte) []*PaymentCircuit {
	cm.mtx.RLock()

	var circuits []*PaymentCircuit
	if circuitSet, ok := cm.hashIndex[hash]; ok {
		circuits = make([]*PaymentCircuit, 0, len(circuitSet))
		for circuit := range circuitSet {
			circuits = append(circuits, &circuit)
		}
	}

	cm.mtx.RUnlock()
	return circuits
}

// Add adds a new active payment circuit to the CircuitMap.
func (cm *CircuitMap) Add(circuit *PaymentCircuit) error {
	cm.mtx.Lock()

	key := circuitKey{
		chanID: circuit.OutgoingChanID,
		htlcID: circuit.OutgoingHTLCID,
	}
	cm.circuits[key] = circuit

	// Add circuit to the hash index.
	if _, ok := cm.hashIndex[circuit.PaymentHash]; !ok {
		cm.hashIndex[circuit.PaymentHash] = make(map[PaymentCircuit]struct{})
	}
	cm.hashIndex[circuit.PaymentHash][*circuit] = struct{}{}

	cm.mtx.Unlock()
	return nil
}

// Remove destroys the target circuit by removing it from the circuit map.
func (cm *CircuitMap) Remove(chanID lnwire.ShortChannelID, htlcID uint64) error {
	cm.mtx.Lock()
	defer cm.mtx.Unlock()

	// Look up circuit so that pointer can be matched in the hash index.
	key := circuitKey{
		chanID: chanID,
		htlcID: htlcID,
	}
	circuit, found := cm.circuits[key]
	if !found {
		return errors.Errorf("Can't find circuit for HTLC %v", key)
	}
	delete(cm.circuits, key)

	// Remove circuit from hash index.
	circuitsWithHash, ok := cm.hashIndex[circuit.PaymentHash]
	if !ok {
		return errors.Errorf("Can't find circuit in hash index for HTLC %v",
			key)
	}

	if _, ok = circuitsWithHash[*circuit]; !ok {
		return errors.Errorf("Can't find circuit in hash index for HTLC %v",
			key)
	}

	delete(circuitsWithHash, *circuit)
	if len(circuitsWithHash) == 0 {
		delete(cm.hashIndex, circuit.PaymentHash)
	}
	return nil
}

// pending returns number of circuits which are waiting for to be completed
// (settle/fail responses to be received).
func (cm *CircuitMap) pending() int {
	cm.mtx.RLock()
	count := len(cm.circuits)
	cm.mtx.RUnlock()
	return count
}
