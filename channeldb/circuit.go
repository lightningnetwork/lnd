package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/wire"
)

var (
	// circuitBucket is the name of the bucket within the database that
	// stores all data related to circuits.
	circuitBucket = []byte("circuits")
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

// FetchAllCircuits fetch all circuits from database.
func (d *DB) FetchAllCircuits() ([]*PaymentCircuit, error) {
	if d == nil {
		return nil, nil
	}

	var circuits []*PaymentCircuit

	err := d.View(func(tx *bolt.Tx) error {
		circuitB := tx.Bucket(circuitBucket)
		if circuitB == nil {
			return ErrNoCircuitsCreated
		}

		// Iterate through the entire key space of the top-level
		// circuit bucket.
		return circuitB.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}

			// Read data from bucket and initialize circuit with it.
			r := bytes.NewReader(v)
			circuit := &PaymentCircuit{}
			if err := deserialize(circuit, r); err != nil {
				return err
			}

			circuits = append(circuits, circuit)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return circuits, nil
}

// AddCircuit places circuit in circuit bucket.
func (d *DB) AddCircuit(circuit *PaymentCircuit) error {
	if d == nil {
		return nil
	}

	return d.Update(func(tx *bolt.Tx) error {
		circuits, err := tx.CreateBucketIfNotExists(circuitBucket)
		if err != nil {
			return err
		}

		key := []byte(circuit.ID())

		// Ensure that a circuit with an identical payment hash doesn't
		// already exist within the index.
		if circuits.Get(key) != nil {
			return ErrDuplicateCircuit
		} else {
			// Finally, serialize the circuit itself to be written
			// to the disk.
			var buf bytes.Buffer
			if err := serialize(circuit, &buf); err != nil {
				return nil
			}

			return circuits.Put(key, buf.Bytes())
		}

	})
}

// RemoveCircuit removes circuit from circuit bucket.
func (d *DB) RemoveCircuit(circuit *PaymentCircuit) error {
	if d == nil {
		return nil
	}

	return d.Update(func(tx *bolt.Tx) error {
		circuits := tx.Bucket(circuitBucket)
		if circuits == nil {
			return ErrCircuitsNotFound
		}

		key := []byte(circuit.ID())
		return circuits.Delete(key)
	})
}
