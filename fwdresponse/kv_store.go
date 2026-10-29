package fwdresponse

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// kvStoreBucketKey is the root level bucket that stores the
	// outgoing CircuitKey -> settle/fail message mapping independently
	// of per-channel forwarding packages.
	kvStoreBucketKey = []byte("fwd-response-store-bucket-key")
)

// KVStore is the KV implementation of Store.
type KVStore struct {
	backend kvdb.Backend
}

var _ Store = (*KVStore)(nil)

// NewKVStore creates a KV-backed forwarding-response store.
func NewKVStore(backend kvdb.Backend) *KVStore {
	return &KVStore{backend: backend}
}

// Wipe removes all forwarding responses in the transaction. This lets the
// database owner include the store in an atomic full-database wipe without
// exposing the store's bucket layout.
func (s *KVStore) Wipe(tx kvdb.RwTx) error {
	err := tx.DeleteTopLevelBucket(kvStoreBucketKey)
	if err == kvdb.ErrBucketNotFound {
		return nil
	}

	return err
}

// ExtractFwdResponses copies eligible settle and fail messages into the
// forwarding response store. It runs in the package-cleanup transaction so the
// two persistence updates remain atomic.
func (s *KVStore) ExtractFwdResponses(tx kvdb.RwTx,
	source lnwire.ShortChannelID) error {

	packager := chanstate.NewChannelPackager(source)

	fwdPkgs, err := packager.LoadFwdPkgs(tx)
	if err != nil {
		return fmt.Errorf("load fwd pkgs for %v: %w", source, err)
	}

	for _, fwdPkg := range fwdPkgs {
		for i, update := range fwdPkg.SettleFails {
			// Responses the incoming link has already locked in
			// need no recovery.
			if fwdPkg.SettleFailFilter.Contains(uint16(i)) {
				continue
			}

			// Only settles and fails are supported.
			var htlcID uint64
			switch msg := update.UpdateMsg.(type) {
			case *lnwire.UpdateFulfillHTLC:
				htlcID = msg.ID

			case *lnwire.UpdateFailHTLC:
				htlcID = msg.ID

			default:
				continue
			}

			outKey := models.CircuitKey{
				ChanID: fwdPkg.Source,
				HtlcID: htlcID,
			}

			err := putFwdResponse(tx, &outKey, update.UpdateMsg)
			if err != nil {
				return fmt.Errorf("store response for %v: %w",
					outKey, err)
			}
		}
	}

	return nil
}

// putFwdResponse writes a single response into the store under its outgoing
// circuit key.
func putFwdResponse(tx kvdb.RwTx, outKey *models.CircuitKey,
	msg lnwire.Message) error {

	var buf bytes.Buffer
	if err := serializeFwdResponse(&buf, msg); err != nil {
		return err
	}

	bucket, err := tx.CreateTopLevelBucket(kvStoreBucketKey)
	if err != nil {
		return err
	}

	return bucket.Put(outKey.Bytes(), buf.Bytes())
}

// FetchFwdResponses returns every response retained for a closed channel. The
// switch calls this on start-up to replay them to their incoming links.
func (s *KVStore) FetchFwdResponses() ([]*Response, error) {

	var responses []*Response

	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(kvStoreBucketKey)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			outKey := models.CircuitKey{}
			err := outKey.SetBytes(k)
			if err != nil {
				return fmt.Errorf("decode response key: %w",
					err)
			}

			msg, err := deserializeFwdResponse(bytes.NewReader(v))
			if err != nil {
				return fmt.Errorf("response for %v: %w",
					outKey, err)
			}

			responses = append(responses, &Response{
				OutKey:  outKey,
				Message: msg,
			})

			return nil
		})
	}, func() {
		responses = nil
	})
	if err != nil {
		return nil, err
	}

	return responses, nil
}

// CheckFwdResponse returns nil if a response is retained for the given
// outgoing circuit key, fwdresponse.ErrNotFound if it is cleanly absent, and
// any database error unchanged. Closed-channel circuit cleanup relies on that
// distinction to avoid deleting the circuit a retained response needs.
func (s *KVStore) CheckFwdResponse(
	outKey *models.CircuitKey) error {

	return kvdb.View(s.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(kvStoreBucketKey)
		if bucket == nil {
			return ErrNotFound
		}

		if bucket.Get(outKey.Bytes()) == nil {
			return ErrNotFound
		}

		return nil
	}, func() {})
}

// DeleteFwdResponse removes the response retained for the given outgoing
// circuit key. The switch calls this only from startup replay, for records
// whose circuit no longer exists. Records that were delivered successfully
// are not deleted at delivery time; they are reaped on the next startup, once
// their circuit has been torn down.
func (s *KVStore) DeleteFwdResponse(
	outKey *models.CircuitKey) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(kvStoreBucketKey)
		if bucket == nil {
			return nil
		}

		return bucket.Delete(outKey.Bytes())
	}, func() {})
}

// serializeFwdResponse writes a settle or fail to the passed io.Writer using
// the wire encoding, which carries the message type along with any custom
// records the outgoing peer attached.
func serializeFwdResponse(w *bytes.Buffer, msg lnwire.Message) error {
	switch msg.(type) {
	case *lnwire.UpdateFulfillHTLC, *lnwire.UpdateFailHTLC:

	default:
		return fmt.Errorf("%w: %T", ErrUnknownType, msg)
	}

	_, err := lnwire.WriteMessage(w, msg, 0)

	return err
}

// deserializeFwdResponse reads a settle or fail from the passed io.Reader.
func deserializeFwdResponse(r io.Reader) (lnwire.Message, error) {
	msg, err := lnwire.ReadMessage(r, 0)
	if err != nil {
		return nil, err
	}

	switch msg.(type) {
	case *lnwire.UpdateFulfillHTLC, *lnwire.UpdateFailHTLC:
		return msg, nil

	default:
		return nil, fmt.Errorf("%w: %T", ErrUnknownType, msg)
	}
}
