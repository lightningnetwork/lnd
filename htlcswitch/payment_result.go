package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
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

	// ErrAttemptResultPending is returned if we try to get a result
	// for a pending payment whose result is not yet available.
	ErrAttemptResultPending = errors.New(
		"attempt result not yet available",
	)

	// ErrAmbiguousAttemptInit is returned when any internal error (eg:
	// with db read or write) occurs during an InitAttempt call that
	// prevents a definitive outcome. This indicates that the state of the
	// payment attempt's inititialization is unknown. Callers should retry
	// the operation to resolve the ambiguity.
	ErrAmbiguousAttemptInit = errors.New(
		"ambiguous result for payment attempt registration",
	)
)

const (
	// pendingHtlcMsgType is a custom message type used to represent a
	// pending HTLC in the network result store.
	pendingHtlcMsgType lnwire.MessageType = 32768
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

	// EncryptedError will contain the raw bytes of an encrypted error
	// in the event of a payment failure if the switch is instructed to
	// defer error processing to external sub-systems.
	EncryptedError []byte
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

	// attemptIDMtx is a multimutex used to serialize operations for a
	// given attempt ID. It ensures InitAttempt's idempotency by protecting
	// its read-then-write sequence from concurrent calls, and it maintains
	// consistency between the database state and result subscribers.
	attemptIDMtx *multimutex.Mutex[uint64]

	// storeMtx is a read-write mutex that protects the entire store during
	// global operations, such as a full cleanup. A read-lock should be
	// held by all per-attempt operations, while a full write-lock should
	// be held by CleanStore.
	storeMtx sync.RWMutex
}

func newNetworkResultStore(db kvdb.Backend) *networkResultStore {
	return &networkResultStore{
		backend:      db,
		results:      make(map[uint64][]chan *networkResult),
		attemptIDMtx: multimutex.NewMutex[uint64](),
	}
}

// InitAttempt initializes the payment attempt with the given attemptID.
//
// If any record (even a pending result placeholder) already exists in the
// store, this method returns ErrPaymentIDAlreadyExists. This guarantees that
// only one HTLC will be initialized and dispatched for a given attempt ID until
// the ID is explicitly cleaned from attempt store.
//
// If any unexpected internal error occurs (such as a database read or write
// failure), it will be wrapped in ErrAmbiguousAttemptInit. This signals
// to the caller that the state of the registration is uncertain and that the
// operation MUST be retried to resolve the ambiguity.
//
// NOTE: This is part of the AttemptStore interface. Subscribed clients do not
// receive notice of this initialization.
func (store *networkResultStore) InitAttempt(attemptID uint64) error {
	// Acquire a lock to protect this fine-grained operation against a
	// concurrent, store-wide CleanStore operation.
	store.storeMtx.Lock()
	defer store.storeMtx.Unlock()

	// We get a mutex for this attempt ID to serialize init, store, and
	// subscribe operations. This is needed to ensure consistency between
	// the database state and the subscribers in case of concurrent calls.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		// Check if any attempt by this ID is already initialized or
		// whether a result for the attempt exists in the store.
		existingResult, err := fetchResult(tx, attemptID)
		if err != nil && !errors.Is(err, ErrPaymentIDNotFound) {
			return err
		}

		// If the result is already in-progress, return an error
		// indicating that the attempt already exists.
		if existingResult != nil {
			log.Warnf("Already initialized attempt for ID=%v",
				attemptID)

			return ErrPaymentIDAlreadyExists
		}

		// Create an empty networkResult to serve as place holder until
		// a result from the network is received.
		//
		// TODO(calvin): When migrating to native SQL storage, replace
		// this custom message placeholder with a proper status enum or
		// struct to represent the pending state of an attempt.
		pendingMsg, err := lnwire.NewCustom(pendingHtlcMsgType, nil)
		if err != nil {
			// This should not happen with a static message type,
			// but if it does, it's an internal error that prevents
			// a definitive outcome, so we must treat it as
			// ambiguous.
			return err
		}
		inProgressResult := &networkResult{
			msg:          pendingMsg,
			unencrypted:  true,
			isResolution: false,
		}

		var b bytes.Buffer
		err = serializeNetworkResult(&b, inProgressResult)
		if err != nil {
			return err
		}

		var attemptIDBytes [8]byte
		binary.BigEndian.PutUint64(attemptIDBytes[:], attemptID)

		// Mark an attempt with this ID as having been seen by storing
		// the pending placeholder. No network result is available yet,
		// so we do not notify subscribers.
		bucket, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		return bucket.Put(attemptIDBytes[:], b.Bytes())
	}, func() {
		// No need to reset existingResult here as it's scoped to the
		// transaction.
	})

	if err != nil {
		if errors.Is(err, ErrPaymentIDAlreadyExists) {
			return ErrPaymentIDAlreadyExists
		}

		// If any unexpected internal error occurs (such as a database
		// failure), it will be wrapped in ErrAmbiguousAttemptInit.
		// This signals to the caller that the state of the attempt
		// initialization is uncertain and that the operation MUST be
		// retried to resolve the ambiguity.
		return fmt.Errorf("%w: %w", ErrAmbiguousAttemptInit, err)
	}

	log.Debugf("Initialized attempt for local payment with attemptID=%v",
		attemptID)

	return nil
}

// StoreResult stores the networkResult for the given attemptID, and notifies
// any subscribers.
func (store *networkResultStore) StoreResult(attemptID uint64,
	result *networkResult) error {

	// Acquire a lock to protect this fine-grained operation against a
	// concurrent, store-wide CleanStore operation.
	store.storeMtx.Lock()
	defer store.storeMtx.Unlock()

	// We get a mutex for this attempt ID. This is needed to ensure
	// consistency between the database state and the subscribers in case
	// of concurrent calls.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	log.Debugf("Storing result for attemptID=%v", attemptID)

	if err := store.storeResult(attemptID, result); err != nil {
		return err
	}

	store.notifySubscribers(attemptID, result)

	return nil
}

// storeResult persists the given result to the database.
func (store *networkResultStore) storeResult(attemptID uint64,
	result *networkResult) error {

	var b bytes.Buffer
	if err := serializeNetworkResult(&b, result); err != nil {
		return err
	}

	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], attemptID)

	return kvdb.Batch(store.backend, func(tx kvdb.RwTx) error {
		networkResults, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		return networkResults.Put(attemptIDBytes[:], b.Bytes())
	})
}

// notifySubscribers notifies any subscribers of the final result for the given
// attempt ID.
func (store *networkResultStore) notifySubscribers(attemptID uint64,
	result *networkResult) {

	store.resultsMtx.Lock()
	for _, res := range store.results[attemptID] {
		res <- result
	}

	delete(store.results, attemptID)
	store.resultsMtx.Unlock()
}

// SubscribeResult is used to get the HTLC attempt result for the given attempt
// ID.  It returns a channel on which the result will be delivered when ready.
func (store *networkResultStore) SubscribeResult(attemptID uint64) (
	<-chan *networkResult, error) {

	// Acquire a read lock to protect this fine-grained operation against
	// a concurrent, store-wide CleanStore operation.
	store.storeMtx.RLock()
	defer store.storeMtx.RUnlock()

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

	// If a result is back from the network, we can send it on the result
	// channel immediately. If the result is still our initialized place
	// holder, then treat it as not yet available.
	if result != nil {
		if result.msg.MsgType() != pendingHtlcMsgType {
			log.Debugf("Obtained full result for attemptID=%v",
				attemptID)

			resultChan <- result

			return resultChan, nil
		}

		log.Debugf("Awaiting result (settle/fail) for attemptID=%v",
			attemptID)
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

// GetResult attempts to immediately fetch the *final* network result for the
// given attempt ID from the store.
//
// NOTE: This method will return ErrAttemptResultPending for attempts
// that have been initialized via InitAttempt but for which a final result
// (settle/fail) has not yet been stored. ErrPaymentIDNotFound is returned
// for attempts that are unknown.
func (store *networkResultStore) GetResult(pid uint64) (
	*networkResult, error) {

	// Acquire a read lock to protect this fine-grained operation against
	// a concurrent, store-wide CleanStore operation.
	store.storeMtx.RLock()
	defer store.storeMtx.RUnlock()

	var result *networkResult
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		result, err = fetchResult(tx, pid)
		if err != nil {
			return err
		}

		// If the attempt is still in-flight, treat it as not yet
		// available to preserve existing expectation for the behavior
		// of this method.
		if result.msg.MsgType() == pendingHtlcMsgType {
			return ErrAttemptResultPending
		}

		return nil
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

// CleanStore removes all entries from the store, except the payment IDs given.
// NOTE: Since every result not listed in the keep map will be deleted, care
// should be taken to ensure no new payment attempts are being made
// concurrently while this process is ongoing, as its result might end up being
// deleted.
func (store *networkResultStore) CleanStore(keep map[uint64]struct{}) error {
	// The CleanStore operation is coarse-grained ("snapshot-and-delete")
	// and must acquire a full write lock to serialize it against all
	// fine-grained, per-attempt operations, preventing race conditions.
	// NOTE: An alternative DeleteAttempts API would allow for more
	// fine-grained locking.
	store.storeMtx.Lock()
	defer store.storeMtx.Unlock()

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

// FetchPendingAttempts returns a list of all attempt IDs that are currently in
// the pending state.
//
// NOTE: This function is NOT safe for concurrent access.
func (store *networkResultStore) FetchPendingAttempts() ([]uint64, error) {
	// Acquire a read lock to protect this fine-grained operation against
	// a concurrent, store-wide CleanStore operation.
	store.storeMtx.RLock()
	defer store.storeMtx.RUnlock()

	var pending []uint64
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(networkResultStoreBucketKey)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			// If the key is not 8 bytes, it's not a valid attempt
			// ID.
			if len(k) != 8 {
				log.Warnf("Found invalid key of length %d in "+
					"network result store", len(k))

				return nil
			}

			// Deserialize the result to check its type.
			r := bytes.NewReader(v)
			result, err := deserializeNetworkResult(r)
			if err != nil {
				// If we can't deserialize, we'll log it and
				// continue. The result will be removed by a
				// call to CleanStore.
				log.Warnf("Unable to deserialize result for "+
					"key %x: %v", k, err)

				return nil
			}

			// If the result is a pending result, add the attempt
			// ID to our list.
			if result.msg.MsgType() == pendingHtlcMsgType {
				attemptID := binary.BigEndian.Uint64(k)
				pending = append(pending, attemptID)
			}

			return nil
		})
	}, func() {
		pending = nil
	})
	if err != nil {
		return nil, err
	}

	return pending, nil
}

// FailPendingAttempt transitions an initialized attempt from a `pending` to a
// `failed` state, recording the provided reason. This ensures that attempts
// which fail before being committed to the forwarding engine are properly
// finalized. This transition unblocks any subscribers that might be waiting on
// a final outcome for an initialized but un-dispatched attempt.
//
// NOTE: This method is specifically for attempts that have been initialized
// via InitAttempt but fail *before* being dispatched to the network. Normal
// failures (e.g., from an HTLC being failed on-chain or by a peer) are
// recorded via the StoreResult method and should not use FailPendingAttempt.
func (store *networkResultStore) FailPendingAttempt(attemptID uint64,
	linkErr *LinkError) error {

	// Acquire a lock to protect this fine-grained operation against a
	// concurrent, store-wide CleanStore operation.
	store.storeMtx.Lock()
	defer store.storeMtx.Unlock()

	// We get a mutex for this attempt ID to ensure consistency between the
	// database state and the subscribers in case of concurrent calls.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	// First, create the failure result.
	failureResult, err := newInternalFailureResult(linkErr)
	if err != nil {
		return fmt.Errorf("failed to create failure message for "+
			"attempt %d: %w", attemptID, err)
	}

	var b bytes.Buffer
	if err := serializeNetworkResult(&b, failureResult); err != nil {
		return fmt.Errorf("failed to serialize failure result: %w", err)
	}
	serializedFailureResult := b.Bytes()

	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], attemptID)

	err = kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		// Verify that the attempt exists and is in the pending
		// state, otherwise we should not fail it.
		existingResult, err := fetchResult(tx, attemptID)
		if err != nil {
			return err
		}

		if existingResult.msg.MsgType() != pendingHtlcMsgType {
			return fmt.Errorf("attempt %d not in pending state",
				attemptID)
		}

		// Write the failure result to the store to unblock any
		// subscribers awaiting a final result.
		bucket, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		return bucket.Put(attemptIDBytes[:], serializedFailureResult)
	}, func() {
	})

	if err != nil {
		return fmt.Errorf("failed to fail pending attempt %d: %w",
			attemptID, err)
	}

	// Lastly, update any subscribers which may be waiting on the result
	// of this attempt.
	store.notifySubscribers(attemptID, failureResult)

	return nil
}

// newInternalFailureResult creates a networkResult representing a terminal,
// internally generated failure.
func newInternalFailureResult(linkErr *LinkError) (*networkResult, error) {
	// First, we need to serialize the wire message from our link error
	// into a byte slice. This is what the downstream parsers expect.
	var reasonBytes bytes.Buffer
	wireMsg := linkErr.WireMessage()
	if err := lnwire.EncodeFailure(&reasonBytes, wireMsg, 0); err != nil {
		return nil, err
	}

	// We'll create a synthetic UpdateFailHTLC to represent this internal
	// failure, following the pattern used by the contract resolver.
	failMsg := &lnwire.UpdateFailHTLC{
		Reason: lnwire.OpaqueReason(reasonBytes.Bytes()),
	}

	return &networkResult{
		msg: failMsg,
		// This is a local failure.
		unencrypted: true,
	}, nil
}
