package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"encoding/binary"

	"io"

	"github.com/boltdb/bolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrPaymentCircuitNotFound is returned if payment circuit haven't been
	// found.
	ErrPaymentCircuitNotFound = errors.New("payment circuit haven't been found")

	// circuitBucketKey name of the bucket which stores the payment circuits.
	circuitBucketKey = []byte("circuitbucket")

	// counterCircuitBucketKey name of the bucket which stores the number of
	// the circuits with the same key.
	counterCircuitBucketKey = []byte("circuitcounters")
)

const (
	// mockObfuscatorCode represents the mock implementation of the
	// obfuscator and is used to understand that we should use the
	// constructor of mock obfuscator implementation.
	mockObfuscatorCode byte = 0

	// failureObfuscatorCode represents the real sphinx implementation of the
	// obfuscator and is used to understand that we should use the
	// constructor of failure/sphinx obfuscator implementation.
	failureObfuscatorCode byte = 1
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

	// Obfuscator is used to re-encrypt the onion failure before sending it
	// back to the originator of the payment.
	Obfuscator Obfuscator
}

// newPaymentCircuit creates new payment circuit instance.
func newPaymentCircuit(src, dest lnwire.ShortChannelID, key circuitKey,
	obfuscator Obfuscator) *paymentCircuit {
	return &paymentCircuit{
		Src:         src,
		Dest:        dest,
		PaymentHash: key,
		Obfuscator:  obfuscator,
	}
}

// isEqual checks the equality of two payment circuits.
func (c *paymentCircuit) isEqual(b *paymentCircuit) bool {
	return bytes.Equal(c.PaymentHash[:], b.PaymentHash[:]) &&
		c.Src == b.Src &&
		c.Dest == b.Dest
}

// Decode reads the data from the byte stream and initialize the
// payment circuit with data.
func (c *paymentCircuit) Decode(r io.Reader) error {
	if _, err := r.Read(c.PaymentHash[:]); err != nil {
		return err
	}

	var srcBytes [8]byte
	if _, err := r.Read(srcBytes[:]); err != nil {
		return err
	}
	c.Src = lnwire.NewShortChanIDFromInt(binary.BigEndian.Uint64(srcBytes[:]))

	var destBytes [8]byte
	if _, err := r.Read(destBytes[:]); err != nil {
		return err
	}
	c.Dest = lnwire.NewShortChanIDFromInt(binary.BigEndian.Uint64(destBytes[:]))

	var code [1]byte
	if _, err := r.Read(code[:]); err != nil {
		return err
	}

	switch code[0] {
	case failureObfuscatorCode:
		c.Obfuscator = &FailureObfuscator{}
	case mockObfuscatorCode:
		c.Obfuscator = &mockObfuscator{}
	default:
		return errors.New("unknown obfuscator type")
	}

	return c.Obfuscator.Decode(r)
}

// Encode writes the internal representation of payment circuit in byte stream.
func (c *paymentCircuit) Encode(w io.Writer) error {
	if _, err := w.Write(c.PaymentHash[:]); err != nil {
		return err
	}

	var srcBytes [8]byte
	binary.BigEndian.PutUint64(srcBytes[:], c.Src.ToUint64())
	if _, err := w.Write(srcBytes[:]); err != nil {
		return err
	}

	var destBytes [8]byte
	binary.BigEndian.PutUint64(destBytes[:], c.Dest.ToUint64())
	if _, err := w.Write(destBytes[:]); err != nil {
		return err
	}

	var code [1]byte
	switch c.Obfuscator.(type) {
	case *FailureObfuscator:
		code[0] = failureObfuscatorCode
	case *mockObfuscator:
		code[0] = mockObfuscatorCode
	default:
		return errors.New("unknown obfuscator type")
	}

	if _, err := w.Write(code[:]); err != nil {
		return err
	}

	return c.Obfuscator.Encode(w)
}

// Key uniquely identifies an active circuit between two open channels.
// Currently, the payment hash is used to uniquely identify each circuit.
// TODO(roasbeef): include dest+src+amt in key
func (c *paymentCircuit) Key() circuitKey {
	return circuitKey(c.PaymentHash)
}

// circuitStorage is a data structure that implements thread safe storage of
// circuits. Each circuit key (payment hash) may have several of circuits
// corresponding to it due to the possibility of repeated payment hashes.
type circuitStorage struct {
	db *channeldb.DB
}

// newCircuitMap creates a new instance of the circuitStorage.
func newCircuitMap(db *channeldb.DB) *circuitStorage {
	return &circuitStorage{db}
}

// add adds a new active payment circuit to the circuitStorage.
func (m *circuitStorage) add(incomingCircuit *paymentCircuit) error {
	return m.db.Batch(func(tx *bolt.Tx) error {
		key := incomingCircuit.Key()
		circuitBucket, err := tx.CreateBucketIfNotExists(circuitBucketKey)
		if err != nil {
			return err
		}

		counterBucket, err := tx.CreateBucketIfNotExists(counterCircuitBucketKey)
		if err != nil {
			return err
		}

		// Examine the circuit map to see if this circuit is already in use or
		// not. If so, then we'll simply increment the reference count.
		// Otherwise, we'll add new circuit in storage.
		refCounter := uint64(1)
		if data := counterBucket.Get(key[:]); data != nil {
			refCounter = binary.BigEndian.Uint64(data)
			refCounter++
		}

		// Update reference counter of the payment circuit.
		var refCounterBytes [8]byte
		binary.BigEndian.PutUint64(refCounterBytes[:], refCounter)
		if err := counterBucket.Put(key[:], refCounterBytes[:]); err != nil {
			return err
		}

		// If object have been putted in the storage already than exit,
		// otherwise encode it and put it in the storage.
		if refCounter != 1 {
			return nil
		}

		// Encode the objects and place it in the circuitBucket.
		var b bytes.Buffer
		if err := incomingCircuit.Encode(&b); err != nil {
			return err
		}

		return circuitBucket.Put(key[:], b.Bytes())
	})
}

// remove destroys the target circuit by removing it from the circuit storage.
func (m *circuitStorage) remove(key circuitKey) (*paymentCircuit, error) {
	existedCircuit := &paymentCircuit{}
	return existedCircuit, m.db.Batch(func(tx *bolt.Tx) error {
		// Get or create the top circuitBucket.
		circuitBucket := tx.Bucket(circuitBucketKey)
		if circuitBucket == nil {
			return ErrPaymentCircuitNotFound
		}

		counterBucket := tx.Bucket(counterCircuitBucketKey)
		if counterBucket == nil {
			return ErrPaymentCircuitNotFound
		}

		var refCounter uint64
		if data := counterBucket.Get(key[:]); data != nil {
			refCounter = binary.BigEndian.Uint64(data)
		} else {
			return ErrPaymentCircuitNotFound
		}

		// Retrieve the payment circuit to return it for the function.
		data := circuitBucket.Get(key[:])
		if data == nil {
			return ErrPaymentCircuitNotFound
		}

		r := bytes.NewReader(data[:])
		if err := existedCircuit.Decode(r); err != nil {
			return err
		}

		if refCounter--; refCounter == 0 {
			if err := circuitBucket.Delete(key[:]); err != nil {
				return err
			}
		}

		var refCounterBytes [8]byte
		binary.BigEndian.PutUint64(refCounterBytes[:], refCounter)
		return counterBucket.Put(key[:], refCounterBytes[:])
	})
}

// pending returns number of circuits which are waiting for to be completed
// (settle/fail responses to be received).
func (m *circuitStorage) pending() int {
	var lengthCircuits uint64
	m.db.View(func(tx *bolt.Tx) error {
		counterBucket := tx.Bucket(counterCircuitBucketKey)
		if counterBucket == nil {
			return nil
		}

		// Iterate over objects buckets.
		return counterBucket.ForEach(func(k, data []byte) error {
			// Skip buckets fields.
			if data == nil {
				return nil
			}

			// Using get instance handler return an empty storable instance and
			// populate it with decoded data.
			lengthCircuits += binary.BigEndian.Uint64(data)
			return nil
		})
	})
	return int(lengthCircuits)
}
