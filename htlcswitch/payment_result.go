package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/multimutex"
)

var (

	// networkResultStoreBucketKey is used for the root level bucket that
	// stores the network result for each payment ID.
	networkResultStoreBucketKey = []byte("network-result-store-bucket")

	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPaymentIDAlreadyExists is returned if we try to write a pending
	// payment whose paymentID already exists.
	ErrPaymentIDAlreadyExists = errors.New("paymentID already exists")
)

// PaymentResult wraps a decoded result received from the network after a
// payment attempt was made. This is what is eventually handed to the router
// for processing.
type PaymentResult struct {
	// Preimage is set by the switch in case a sent HTLC was settled.
	Preimage [32]byte

	// Error is non-nil in case a HTLC send failed, and the HTLC is now
	// irrevocably canceled. If the payment failed during forwarding, this
	// error will be a *ForwardingError.
	Error error
}

// networkResult is the raw result received from the network after a payment
// attempt has been made. Since the switch doesn't always have the necessary
// data to decode the raw message, we store it together with some meta data,
// and decode it when the router query for the final result.
type networkResult struct {
	// msg is the received result. This should be of type UpdateFulfillHTLC
	// or UpdateFailHTLC.
	msg lnwire.Message

	// unencrypted indicates whether the failure encoded in the message is
	// unencrypted, and hence doesn't need to be decrypted.
	unencrypted bool

	// isResolution indicates whether this is a resolution message, in
	// which the failure reason might not be included.
	isResolution bool
}

// serializeNetworkResult serializes the networkResult.
func serializeNetworkResult(w io.Writer, n *networkResult) error {
	return channeldb.WriteElements(w, n.msg, n.unencrypted, n.isResolution)
}

// deserializeNetworkResult deserializes the networkResult.
func deserializeNetworkResult(r io.Reader) (*networkResult, error) {
	n := &networkResult{}

	if err := channeldb.ReadElements(r,
		&n.msg, &n.unencrypted, &n.isResolution,
	); err != nil {
		return nil, err
	}

	return n, nil
}

// networkResultStore is a persistent store that stores any results of HTLCs in
// flight on the network. Since payment results are inherently asynchronous, it
// is used as a common access point for senders of HTLCs, to know when a result
// is back. The Switch will checkpoint any received result to the store, and
// the store will keep results and notify the callers about them.
type networkResultStore struct {
	backend kvdb.Backend

	// results is a map from paymentIDs to channels where subscribers to
	// payment results will be notified.
	results    map[uint64][]chan *networkResult
	resultsMtx sync.Mutex

	// attemptIDMtx is a multimutex used to make sure the database and
	// result subscribers map is consistent for each attempt ID in case of
	// concurrent callers.
	attemptIDMtx *multimutex.Mutex[uint64]
}

func newNetworkResultStore(db kvdb.Backend) *networkResultStore {
	return &networkResultStore{
		backend:      db,
		results:      make(map[uint64][]chan *networkResult),
		attemptIDMtx: multimutex.NewMutex[uint64](),
	}
}

// storeResult stores the networkResult for the given attemptID, and notifies
// any subscribers.
func (store *networkResultStore) storeResult(attemptID uint64,
	result *networkResult) error {

	// We get a mutex for this attempt ID. This is needed to ensure
	// consistency between the database state and the subscribers in case
	// of concurrent calls.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	log.Debugf("Storing result for attemptID=%v", attemptID)

	// Serialize the payment result.
	var b bytes.Buffer
	if err := serializeNetworkResult(&b, result); err != nil {
		return err
	}

	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], attemptID)

	err := kvdb.Batch(store.backend, func(tx kvdb.RwTx) error {
		networkResults, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		return networkResults.Put(attemptIDBytes[:], b.Bytes())
	})
	if err != nil {
		return err
	}

	// Now that the result is stored in the database, we can notify any
	// active subscribers.
	store.resultsMtx.Lock()
	for _, res := range store.results[attemptID] {
		res <- result
	}
	delete(store.results, attemptID)
	store.resultsMtx.Unlock()

	return nil
}

// subscribeResult is used to get the HTLC attempt result for the given attempt
// ID.  It returns a channel on which the result will be delivered when ready.
func (store *networkResultStore) subscribeResult(attemptID uint64) (
	<-chan *networkResult, error) {

	// We get a mutex for this payment ID. This is needed to ensure
	// consistency between the database state and the subscribers in case
	// of concurrent calls.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	log.Debugf("Subscribing to result for attemptID=%v", attemptID)

	var (
		result     *networkResult
		resultChan = make(chan *networkResult, 1)
	)

	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		result, err = fetchResult(tx, attemptID)
		switch {

		// Result not yet available, we will notify once a result is
		// available.
		case err == ErrPaymentIDNotFound:
			return nil

		case err != nil:
			return err

		// The result was found, and will be returned immediately.
		default:
			return nil
		}
	}, func() {
		result = nil
	})
	if err != nil {
		return nil, err
	}

	// If the result was found, we can send it on the result channel
	// imemdiately.
	if result != nil {
		resultChan <- result
		return resultChan, nil
	}

	// Otherwise we store the result channel for when the result is
	// available.
	store.resultsMtx.Lock()
	store.results[attemptID] = append(
		store.results[attemptID], resultChan,
	)
	store.resultsMtx.Unlock()

	return resultChan, nil
}

// getResult attempts to immediately fetch the result for the given pid from
// the store. If no result is available, ErrPaymentIDNotFound is returned.
func (store *networkResultStore) getResult(pid uint64) (
	*networkResult, error) {

	var result *networkResult
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		result, err = fetchResult(tx, pid)
		return err
	}, func() {
		result = nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func fetchResult(tx kvdb.RTx, pid uint64) (*networkResult, error) {
	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], pid)

	networkResults := tx.ReadBucket(networkResultStoreBucketKey)
	if networkResults == nil {
		return nil, ErrPaymentIDNotFound
	}

	// Check whether a result is already available.
	resultBytes := networkResults.Get(attemptIDBytes[:])
	if resultBytes == nil {
		return nil, ErrPaymentIDNotFound
	}

	// Decode the result we found.
	r := bytes.NewReader(resultBytes)

	return deserializeNetworkResult(r)
}

// cleanStore removes all entries from the store, except the payment IDs given.
// NOTE: Since every result not listed in the keep map will be deleted, care
// should be taken to ensure no new payment attempts are being made
// concurrently while this process is ongoing, as its result might end up being
// deleted.
func (store *networkResultStore) cleanStore(keep map[uint64]struct{}) error {
	return kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		networkResults, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		// Iterate through the bucket, deleting all items not in the
		// keep map.
		var toClean [][]byte
		if err := networkResults.ForEach(func(k, _ []byte) error {
			pid := binary.BigEndian.Uint64(k)
			if _, ok := keep[pid]; ok {
				return nil
			}

			toClean = append(toClean, k)
			return nil
		}); err != nil {
			return err
		}

		for _, k := range toClean {
			err := networkResults.Delete(k)
			if err != nil {
				return err
			}
		}

		if len(toClean) > 0 {
			log.Infof("Removed %d stale entries from network "+
				"result store", len(toClean))
		}

		return nil
	}, func() {})
}
