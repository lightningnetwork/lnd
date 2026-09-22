package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// fwdResponseBucketKey is the root level bucket that stores the
	// outgoing CircuitKey -> settle/fail message mapping independently
	// of per-channel forwarding packages.
	fwdResponseBucketKey = []byte("fwd-response-store-bucket-key")

	// ErrFwdResponseNotFound identifies a clean forwarding response store
	// miss. Callers must not treat other storage errors as proof that no
	// response exists.
	ErrFwdResponseNotFound = errors.New("forwarding response not found")

	// ErrUnknownFwdResponse signals that a stored response deserialized
	// into a message that is neither a settle nor a fail.
	ErrUnknownFwdResponse = errors.New("unknown forwarding response type")
)

// FwdResponse is an off-chain settle or fail retained under its outgoing
// circuit key. This gives forwarding responses the same circuit-oriented
// lookup model used by contractcourt resolution messages.
type FwdResponse struct {
	// OutKey identifies the outgoing HTLC that this response resolves.
	OutKey models.CircuitKey

	// Message is the wire response received from the outgoing peer. Keeping
	// the message intact allows the switch to use its regular response path
	// during replay.
	Message lnwire.Message
}

// extractFwdResponses copies eligible settle and fail messages into the
// forwarding response store. It runs in the package-cleanup transaction so the
// two persistence updates remain atomic.
func extractFwdResponses(tx kvdb.RwTx, source lnwire.ShortChannelID) error {
	packager := NewChannelPackager(source)

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
			// Log any unexpected update type.
			var htlcID uint64
			switch msg := update.UpdateMsg.(type) {
			case *lnwire.UpdateFulfillHTLC:
				htlcID = msg.ID

			case *lnwire.UpdateFailHTLC:
				htlcID = msg.ID

			default:
				log.Warnf("Skipping unsupported response "+
					"during close: type=%T, source=%v, "+
					"height=%d, index=%d", update.UpdateMsg,
					fwdPkg.Source, fwdPkg.Height, i)

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

	bucket, err := tx.CreateTopLevelBucket(fwdResponseBucketKey)
	if err != nil {
		return err
	}

	return bucket.Put(outKey.Bytes(), buf.Bytes())
}

// FetchFwdResponses returns every response retained for a closed channel. The
// switch calls this on start-up to replay them to their incoming links.
func (c *ChannelStateDB) FetchFwdResponses() ([]*FwdResponse, error) {
	var responses []*FwdResponse

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(fwdResponseBucketKey)
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

			responses = append(responses, &FwdResponse{
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
// outgoing circuit key, ErrFwdResponseNotFound if it is cleanly absent, and
// any database error unchanged. Closed-channel circuit cleanup relies on that
// distinction to avoid deleting the circuit a retained response needs.
func (c *ChannelStateDB) CheckFwdResponse(outKey *models.CircuitKey) error {
	return kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(fwdResponseBucketKey)
		if bucket == nil {
			return ErrFwdResponseNotFound
		}

		if bucket.Get(outKey.Bytes()) == nil {
			return ErrFwdResponseNotFound
		}

		return nil
	}, func() {})
}

// DeleteFwdResponse removes the response retained for the given outgoing
// circuit key. The switch calls this only from startup replay, for records
// whose circuit no longer exists. Records that were delivered successfully
// are not deleted at delivery time; they are reaped on the next startup, once
// their circuit has been torn down.
func (c *ChannelStateDB) DeleteFwdResponse(
	outKey *models.CircuitKey) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(fwdResponseBucketKey)
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
		return fmt.Errorf("%w: %T", ErrUnknownFwdResponse, msg)
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
		return nil, fmt.Errorf("%w: %T", ErrUnknownFwdResponse, msg)
	}
}
