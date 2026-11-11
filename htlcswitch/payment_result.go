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

	// ErrUndrainedResultStore is returned when the result store still
	// holds records written by a switchrpc build that named attempts by
	// the raw attempt ID its client chose.
	ErrUndrainedResultStore = errors.New("result store holds attempts " +
		"of a switchrpc build without attempt keys")

	// legacyLifecycleMarkerBucket is a top-level bucket that switchrpc
	// builds without attempt keys wrote on every start. Its presence means
	// the result store may hold records stored under client chosen attempt
	// IDs, which have no index entry.
	legacyLifecycleMarkerBucket = []byte("external-lifecycle-marker")
)

// DeletionStatus represents the outcome of deleting a single attempt from
// the store.
type DeletionStatus int

const (
	// DeletionOK indicates the attempt was successfully deleted.
	DeletionOK DeletionStatus = iota

	// DeletionPending indicates the attempt is still in-flight and cannot
	// be deleted.
	DeletionPending

	// DeletionNotFound indicates the attempt ID was not found in the
	// store. This may indicate a client bug (wrong ID, wrong backend).
	DeletionNotFound

	// DeletionFailed indicates that the attempt record exists but could
	// not be processed (e.g. deserialization error). The record remains
	// in the store.
	DeletionFailed
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
	// given attempt ID. It maintains consistency between the database
	// state and result subscribers.
	attemptIDMtx *multimutex.Mutex[uint64]

	// externalKeyMtx is a multimutex keyed by the external attempt key.
	// It serializes the index lookup and insert in InitAttempt,
	// so two calls with the same key yield exactly one internal ID.
	externalKeyMtx *multimutex.Mutex[string]
}

func newNetworkResultStore(db kvdb.Backend) *networkResultStore {
	return &networkResultStore{
		backend:        db,
		results:        make(map[uint64][]chan *networkResult),
		attemptIDMtx:   multimutex.NewMutex[uint64](),
		externalKeyMtx: multimutex.NewMutex[string](),
	}
}

// checkLegacyLifecycleMarker refuses to open a result store that a switchrpc
// build without attempt keys left records in. Such a build stored results under
// the attempt IDs its client chose, with no index entry, so this build cannot
// address them by key, cannot tell them apart from its own attempts, and would
// delete them in its startup cleanup. A marked store with no records was
// drained, so the marker is removed and the store opens normally. A store
// without the marker is not inspected further.
func checkLegacyLifecycleMarker(db kvdb.Backend) error {
	// Most stores never carried the marker, so look for it in a read
	// transaction and only take a write transaction when it is there.
	var present bool
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		present = tx.ReadBucket(legacyLifecycleMarkerBucket) != nil

		return nil
	}, func() {
		present = false
	})
	if err != nil {
		return fmt.Errorf("unable to check the external lifecycle "+
			"marker: %w", err)
	}
	if !present {
		return nil
	}

	var (
		marked  bool
		records int
	)
	err = kvdb.Update(db, func(tx kvdb.RwTx) error {
		if tx.ReadBucket(legacyLifecycleMarkerBucket) == nil {
			return nil
		}
		marked = true

		results := tx.ReadBucket(networkResultStoreBucketKey)
		if results != nil {
			err := results.ForEach(func(_, _ []byte) error {
				records++

				return nil
			})
			if err != nil {
				return err
			}
		}

		if records > 0 {
			return nil
		}

		return tx.DeleteTopLevelBucket(legacyLifecycleMarkerBucket)
	}, func() {
		marked = false
		records = 0
	})
	if err != nil {
		return fmt.Errorf("unable to check the external lifecycle "+
			"marker: %w", err)
	}

	if records > 0 {
		return fmt.Errorf("%w: %d record(s) remain. Start the "+
			"previous build, let the client's in-flight attempts "+
			"resolve, delete all remaining attempts with "+
			"DeleteAttempts, then upgrade again",
			ErrUndrainedResultStore, records)
	}

	if marked {
		log.Infof("Removed the external lifecycle marker from the " +
			"drained result store")
	}

	return nil
}

// putPendingPlaceholder writes the pending placeholder record for the given
// attempt ID within the passed transaction. The placeholder marks the attempt
// as initialized until a result from the network is received.
func putPendingPlaceholder(tx kvdb.RwTx, attemptID uint64) error {
	// Create an empty networkResult to serve as place holder until a
	// result from the network is received.
	//
	// TODO(calvin): When migrating to native SQL storage, replace this
	// custom message placeholder with a proper status enum or struct to
	// represent the pending state of an attempt.
	pendingMsg, err := lnwire.NewCustom(pendingHtlcMsgType, nil)
	if err != nil {
		// This should not happen with a static message type, but if it
		// does, it's an internal error that prevents a definitive
		// outcome, so we must treat it as ambiguous.
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

	bucket, err := tx.CreateTopLevelBucket(networkResultStoreBucketKey)
	if err != nil {
		return err
	}

	return bucket.Put(attemptIDBytes[:], b.Bytes())
}

// StoreResult stores the networkResult for the given attemptID, and notifies
// any subscribers.
func (store *networkResultStore) StoreResult(attemptID uint64,
	result *networkResult) error {

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

// CleanStore removes all entries from the store, except the payment IDs given
// and the attempts named in the external attempt index. Indexed attempts
// belong to an external dispatcher, which deletes them by key itself.
//
// NOTE: Since every unindexed result not listed in the keep map will be
// deleted, care should be taken to ensure no new local payment attempts are
// being made concurrently while this process is ongoing, as its result might
// end up being deleted. Externally keyed attempts need no such care: their
// record and index entry are written in one transaction, and the index is
// read in the same transaction as the deletes below.
func (store *networkResultStore) CleanStore(keep map[uint64]struct{}) error {
	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		indexed, err := fetchIndexedAttemptIDs(tx)
		if err != nil {
			return err
		}

		networkResults, err := tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		// Snapshot the entries we want to keep (the results of still
		// in-flight attempts) and count the rest. The store is only
		// pruned here at startup, so it can hold one entry per resolved
		// HTLC attempt and grow very large. Deleting those one key at a
		// time is O(N) round-trips on a SQL backend; since the keep set
		// is small, we instead read the keepers with a single streaming
		// query, drop the whole bucket in one cascading delete, and
		// re-insert the keepers.
		type keptResult struct {
			key   []byte
			value []byte
		}
		var (
			kept    []keptResult
			removed int
		)
		if err := kvdb.ForAll(networkResults, func(k, v []byte) error {
			pid := binary.BigEndian.Uint64(k)
			_, keepListed := keep[pid]
			_, owned := indexed[pid]
			if !keepListed && !owned {
				removed++
				return nil
			}

			// The bucket is deleted below, so clone the retained
			// entries before stashing them.
			kept = append(kept, keptResult{
				key:   bytes.Clone(k),
				value: bytes.Clone(v),
			})

			return nil
		}); err != nil {
			return err
		}

		// If there is nothing to remove, avoid dropping and recreating
		// the bucket.
		if removed == 0 {
			return nil
		}

		// Drop the whole bucket in a single query (on the SQL backend
		// this cascades to every row), then recreate it and restore the
		// entries we are keeping.
		if err := tx.DeleteTopLevelBucket(
			networkResultStoreBucketKey,
		); err != nil {
			return err
		}

		networkResults, err = tx.CreateTopLevelBucket(
			networkResultStoreBucketKey,
		)
		if err != nil {
			return err
		}

		for _, r := range kept {
			err := networkResults.Put(r.key, r.value)
			if err != nil {
				return err
			}
		}

		log.Infof("Removed %d stale entries from network result store",
			removed)

		return nil
	}, func() {})

	// An unreadable index entry may protect any attempt, so we cannot
	// tell which results are safe to remove. Removing nothing is safe,
	// and the store is pruned again on the next start.
	if errors.Is(err, errUnreadableIndex) {
		log.Errorf("Skipping network result store cleanup: %v", err)

		return nil
	}

	return err
}

// FetchPendingAttempts returns a list of all attempt IDs that are currently in
// the pending state.
//
// NOTE: This function is NOT safe for concurrent access.
func (store *networkResultStore) FetchPendingAttempts() ([]uint64, error) {
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
