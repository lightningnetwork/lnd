package htlcswitch

import (
	"bytes"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
)

var (
	// ErrCorruptedCircuitMap indicates that the on-disk bucketing structure
	// has altered since the circuit map instance was initialized.
	ErrCorruptedCircuitMap = errors.New("circuit map has been corrupted")

	// ErrCircuitNotInHashIndex indicates that a particular circuit did not
	// appear in the in-memory hash index.
	ErrCircuitNotInHashIndex = errors.New("payment circuit not found in " +
		"hash index")

	// ErrUnknownCircuit signals that circuit could not be removed from the
	// map because it was not found.
	ErrUnknownCircuit = errors.New("unknown payment circuit")

	// ErrDuplicateCircuit signals that this circuit was previously
	// added.
	ErrDuplicateCircuit = errors.New("duplicate circuit add")

	// ErrUnknownKeystone signals that no circuit was found using the
	// outgoing circuit key.
	ErrUnknownKeystone = errors.New("unknown circuit keystone")

	// ErrDuplicateKeystone signals that this circuit was previously
	// assigned a keystone.
	ErrDuplicateKeystone = errors.New("cannot add duplicate keystone")
)

// CircuitMap is an interface for managing the construction and teardown of
// payment circuits used by the switch.
type CircuitMap interface {
	// CommitCircuits attempts to add the given circuit's to the circuit
	// map, returning a list of the circuits that had not been seen prior.
	CommitCircuits(circuit ...*PaymentCircuit) ([]*PaymentCircuit, error)

	// NumPending returns the total number of active circuits added by
	// CommitCircuits.
	NumPending() int

	// SetKeystone is used to assign the outgoing circuit key to the circuit
	// identified by inKey.
	SetKeystone(inKey CircuitKey, outKey CircuitKey) error

	// NumOpen returns the number of circuits with HTLCs that have been
	// forwarded via an outgoing link.
	NumOpen() int

	// LookupCircuit queries the circuit map for the circuit identified by
	// inKey.
	LookupCircuit(inKey CircuitKey) *PaymentCircuit

	// LookupByKeystone queries the circuit map for a circuit identified by
	// its outgoing circuit key.
	LookupByKeystone(outKey CircuitKey) *PaymentCircuit

	// LookupByPaymentHash queries the circuit map and returns all open
	// circuits that use the given payment hash.
	LookupByPaymentHash(hash [32]byte) []*PaymentCircuit

	// Delete removes the incoming circuit key to remove all references to a
	// circuit.
	Delete(inKey CircuitKey) error
}

var (

	// circuitAddKey is the key used to retrieve the bucket containing
	// payment circuits. A circuit records information about how to return a
	// packet to the source link, potentially including an error encrypter
	// for applying this hop's encryption to the payload in the reverse
	// direction.
	circuitAddKey = []byte("circuit-adds")

	// circuitKeystoneKey is used to retrieve the bucket containing circuit
	// keystones, which are set in place once a forwarded packet is assigned
	// an index on an outgoing commitment txn.
	circuitKeystoneKey = []byte("circuit-keysontes")
)

// circuitMap is a data structure that implements thread safe, persistent
// storage of circuit routing information. The switch consults a circuit map to
// determine where to forward HTLC update messages. Each circuit is stored with
// it's outgoing HTLC as the primary key because, each offered HTLC has at most
// one received HTLC, but there may be multiple offered or received HTLCs with
// the same payment hash. Circuits are also indexed to provide fast lookups by
// payment hash.
type circuitMap struct {
	// db provides the persistent storage engine for the circuit map.
	// TODO(conner): create abstraction to allow for the substitution of
	// other persistence engines.
	db *channeldb.DB

	mtx sync.RWMutex

	// pending is an in-memory mapping of all half payment circuits, and
	// is kept in sync with the on-disk contents of the circuit map.
	pending map[CircuitKey]*PaymentCircuit

	// opened is an in-memory maping of all full payment circuits, which is
	// also synchronized with the persistent state of the circuit map.
	opened map[CircuitKey]*PaymentCircuit

	// hashIndex is a volatile index that facilitates fast queries by
	// payment hash against the contents of circuits. This index can be
	// reconstructed entirely from the set of persisted full circuits on
	// startup.
	hashIndex map[[32]byte]map[CircuitKey]struct{}
}

// NewCircuitMap creates a new instance of the circuitMap.
func NewCircuitMap(db *channeldb.DB) (CircuitMap, error) {
	cm := &circuitMap{
		db: db,
	}

	// Initialize the on-disk buckets used by the circuit map.
	if err := cm.initBuckets(); err != nil {
		return nil, err
	}

	// Load any previously persisted circuit info back into memory.
	if err := cm.restoreMemState(); err != nil {
		return nil, err
	}

	return cm, nil
}

// initBuckets ensures that the primary buckets used by the circuit are
// initialized so that we can assume their existence after startup.
func (cm *circuitMap) initBuckets() error {
	return cm.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(circuitKeystoneKey)
		if err != nil {
			return err
		}

		_, err = tx.CreateBucketIfNotExists(circuitAddKey)
		return err
	})
}

// restoreMemState loads the contents of the half circuit and full circuit buckets
// from disk and reconstructs the in-memory representation of the circuit map.
// Afterwards, the state of the hash index is reconstructed using the recovered
// set of full circuits.
func (cm *circuitMap) restoreMemState() error {

	var (
		opened  = make(map[CircuitKey]*PaymentCircuit)
		pending = make(map[CircuitKey]*PaymentCircuit)
	)

	if err := cm.db.View(func(tx *bolt.Tx) error {
		// Restore any of the circuits persisted in the circuit bucket
		// back into memory.
		circuitBkt := tx.Bucket(circuitAddKey)
		if circuitBkt == nil {
			return ErrCorruptedCircuitMap
		}

		if err := circuitBkt.ForEach(func(_, v []byte) error {
			circuit, err := restoreCircuit(v)
			if err != nil {
				return err
			}

			pending[circuit.Incoming] = circuit

			return nil
		}); err != nil {
			return err
		}

		// Furthermore, load the keystone bucket and resurrect the
		// keystones used in any open circuits.
		keystoneBkt := tx.Bucket(circuitKeystoneKey)
		if keystoneBkt == nil {
			return ErrCorruptedCircuitMap
		}

		if err := keystoneBkt.ForEach(func(k, v []byte) error {
			var (
				inKey  CircuitKey
				outKey = &CircuitKey{}
			)

			if err := inKey.SetBytes(v); err != nil {
				return err
			}
			if err := outKey.SetBytes(k); err != nil {
				return err
			}

			// Retrieve the pending circuit, set its keystone, then
			// add it to the opened map.
			circuit := pending[inKey]
			circuit.Outgoing = outKey
			opened[*outKey] = circuit

			return nil
		}); err != nil {
			return err
		}

		return nil

	}); err != nil {
		return err
	}

	cm.pending = pending
	cm.opened = opened

	// Finally, reconstruct the hash index by running through our set of
	// open circuits.
	cm.hashIndex = make(map[[32]byte]map[CircuitKey]struct{})
	for _, circuit := range opened {
		cm.addCircuitToHashIndex(circuit)
	}

	return nil
}

// restoreCircuit reconstructs an in-memory payment circuit from a byte slice.
// The byte slice is assumed to have been generated by the circuit's Encode
// method.
func restoreCircuit(v []byte) (*PaymentCircuit, error) {
	var circuit = &PaymentCircuit{}

	circuitReader := bytes.NewReader(v)
	if err := circuit.Decode(circuitReader); err != nil {
		return nil, err
	}

	return circuit, nil
}

// LookupByHTLC looks up the payment circuit by the outgoing channel and HTLC
// IDs. Returns nil if there is no such circuit.
func (cm *circuitMap) LookupCircuit(inKey CircuitKey) *PaymentCircuit {
	cm.mtx.RLock()
	defer cm.mtx.RUnlock()

	return cm.pending[inKey]
}

// LookupByKeystone searches for the circuit identified by its outgoing circuit
// key.
func (cm *circuitMap) LookupByKeystone(outKey CircuitKey) *PaymentCircuit {
	cm.mtx.RLock()
	defer cm.mtx.RUnlock()

	return cm.opened[outKey]
}

// LookupByPaymentHash looks up and returns any payment circuits with a given
// payment hash.
func (cm *circuitMap) LookupByPaymentHash(hash [32]byte) []*PaymentCircuit {
	cm.mtx.RLock()
	defer cm.mtx.RUnlock()

	var circuits []*PaymentCircuit
	if circuitSet, ok := cm.hashIndex[hash]; ok {
		// Iterate over the outgoing circuit keys found with this hash,
		// and retrieve the circuit from the opened map.
		circuits = make([]*PaymentCircuit, 0, len(circuitSet))
		for key := range circuitSet {
			if circuit, ok := cm.opened[key]; ok {
				circuits = append(circuits, circuit)
			}
		}
	}

	return circuits
}

// CommitCircuits accepts any number of circuits and persistently adds them to
// the switch's circuit map. The method returns a list of circuits that had not
// been seen prior by the switch. A link should only forward HTLCs corresponding
// to the returned circuits to the switch.
// NOTE: This method uses batched writes to improve performance, gains will only
// be realized if it is called concurrently from separate goroutines. This
// method is intended to be called in the htlcManager of each link.
func (cm *circuitMap) CommitCircuits(
	circuits ...*PaymentCircuit) ([]*PaymentCircuit, error) {

	// If an empty list was passed, return early to avoid grabbing the lock.
	if len(circuits) == 0 {
		return nil, nil
	}

	// First, we reconcile the provided circuits with our set of pending
	// circuits to construct a set of new circuits that need to be written
	// to disk. The circuit's pointer is stored so that we only permit this
	// exact circuit to be forwarded through the switch. If a circuit is
	// already pending, the htlc will be reforwarded by the switch.
	cm.mtx.Lock()
	var circuitsToAdd []*PaymentCircuit
	for _, circuit := range circuits {
		inKey := circuit.InKey()
		if _, ok := cm.pending[inKey]; !ok {
			cm.pending[inKey] = circuit
			circuitsToAdd = append(circuitsToAdd, circuit)
		}
	}
	cm.mtx.Unlock()

	// If all circuits were previously started, we are done.
	if len(circuitsToAdd) == 0 {
		return nil, nil
	}

	// Now, optimistically serialize the circuits to add.
	var bs = make([]bytes.Buffer, len(circuitsToAdd))
	for i, circuit := range circuitsToAdd {
		if err := circuit.Encode(&bs[i]); err != nil {
			return nil, err
		}
	}

	// Write the entire batch of circuits to the persistent circuit bucket
	// using bolt's Batch write. This method must be called from multiple,
	// distinct goroutines to have any impact on performance.
	err := cm.db.Batch(func(tx *bolt.Tx) error {
		circuitBkt := tx.Bucket(circuitAddKey)
		if circuitBkt == nil {
			return ErrCorruptedCircuitMap
		}

		for i, circuit := range circuitsToAdd {
			inKeyBytes := circuit.InKey().Bytes()
			circuitBytes := bs[i].Bytes()

			err := circuitBkt.Put(inKeyBytes, circuitBytes)
			if err != nil {
				return err
			}
		}

		return nil
	})

	// Return if the write succeeded.
	if err == nil {
		return circuitsToAdd, nil
	}

	// Otherwise, rollback the circuits added to the pending set if the
	// write failed.
	cm.mtx.Lock()
	for _, circuit := range circuitsToAdd {
		inKey := circuit.InKey()
		if _, ok := cm.pending[inKey]; !ok {
			delete(cm.pending, inKey)
		}
	}
	cm.mtx.Unlock()

	return nil, err
}

// SetKeystone sets the outgoing circuit key for the circuit identified by
// inKey, persistently marking the circuit as opened. After the changes have
// been persisted, the circuit map's in-memory indexes are updated so that this
// circuit can be queried using LookupByKeystone or LookupByPaymentHash.
func (cm *circuitMap) SetKeystone(inKey, outKey CircuitKey) error {
	cm.mtx.Lock()
	defer cm.mtx.Unlock()

	if _, ok := cm.opened[outKey]; ok {
		return ErrDuplicateKeystone
	}

	circuit, ok := cm.pending[inKey]
	if !ok {
		return ErrUnknownCircuit
	}

	if err := cm.db.Update(func(tx *bolt.Tx) error {
		// Now, load the circuit bucket to which we will write the
		// already serialized circuit.
		keystoneBkt := tx.Bucket(circuitKeystoneKey)
		if keystoneBkt == nil {
			return ErrCorruptedCircuitMap
		}

		return keystoneBkt.Put(outKey.Bytes(), inKey.Bytes())
	}); err != nil {
		return err
	}

	// Since our persistent operation was successful, we can now modify the
	// in memory representations. Set the outgoing circuit key on our
	// pending circuit, add the same circuit to set of opened circuits, and
	// add this circuit to the hash index.
	circuit.Outgoing = &CircuitKey{}
	*circuit.Outgoing = outKey

	cm.opened[outKey] = circuit
	cm.addCircuitToHashIndex(circuit)

	return nil
}

// addCirciutToHashIndex inserts a circuit into the circuit map's hash index, so
// that it can be queried using LookupByPaymentHash.
func (cm *circuitMap) addCircuitToHashIndex(c *PaymentCircuit) {
	if _, ok := cm.hashIndex[c.PaymentHash]; !ok {
		cm.hashIndex[c.PaymentHash] = make(map[CircuitKey]struct{})
	}
	cm.hashIndex[c.PaymentHash][c.OutKey()] = struct{}{}
}

// Delete destroys the target circuit by removing it from the circuit map,
// additionally removing the circuit's keystone if the HTLC was forwarded
// through an outgoing link. The circuit should be identified by its incoming
// circuit key.
func (cm *circuitMap) Delete(inKey CircuitKey) error {
	cm.mtx.Lock()
	defer cm.mtx.Unlock()

	circuit, ok := cm.pending[inKey]
	if !ok {
		return ErrUnknownCircuit
	}

	if err := cm.db.Update(func(tx *bolt.Tx) error {
		// If this htlc made it to an outgoing link, load the keystone
		// bucket from which we will remove the outgoing circuit key.
		if circuit.HasKeystone() {
			keystoneBkt := tx.Bucket(circuitKeystoneKey)
			if keystoneBkt == nil {
				return ErrCorruptedCircuitMap
			}

			outKey := circuit.OutKey()

			err := keystoneBkt.Delete(outKey.Bytes())
			if err != nil {
				return err
			}
		}

		// Remove the circuit itself based on the incoming circuit key.
		circuitBkt := tx.Bucket(circuitAddKey)
		if circuitBkt == nil {
			return ErrCorruptedCircuitMap
		}

		return circuitBkt.Delete(inKey.Bytes())
	}); err != nil {
		return err
	}

	// With the persistent changes made, remove all references to this
	// circuit from memory.
	delete(cm.pending, inKey)

	if circuit.HasKeystone() {
		delete(cm.opened, circuit.OutKey())
		cm.removeCircuitFromHashIndex(circuit)
	}

	return nil
}

// removeCircuitFromHashIndex removes the given circuit from the hash index,
// pruning any unnecessary memory optimistically.
func (cm *circuitMap) removeCircuitFromHashIndex(c *PaymentCircuit) {
	// Locate bucket containing this circuit's payment hashes.
	circuitsWithHash, ok := cm.hashIndex[c.PaymentHash]
	if !ok {
		return
	}

	// Check that this circuit is a member of the set of circuit's with this
	// payment hash.
	outKey := c.OutKey()
	if _, ok = circuitsWithHash[outKey]; !ok {
		return
	}

	// If found, remove this circuit from the set.
	delete(circuitsWithHash, outKey)

	// Prune the payment hash bucket if no other entries remain.
	if len(circuitsWithHash) == 0 {
		delete(cm.hashIndex, c.PaymentHash)
	}
}

// NumPending returns the number of active circuits added to the circuit map.
func (cm *circuitMap) NumPending() int {
	cm.mtx.RLock()
	defer cm.mtx.RUnlock()

	return len(cm.pending)
}

// NumOpen returns the number of circuits that have been opened by way of
// setting their keystones. This is the number of HTLCs that are waiting for a
// settle/fail response from a remote peer.
func (cm *circuitMap) NumOpen() int {
	cm.mtx.RLock()
	defer cm.mtx.RUnlock()

	return len(cm.opened)
}
