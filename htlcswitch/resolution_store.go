package htlcswitch

import (
	"bytes"
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// resBucketKey is used for the root level bucket that stores the
	// CircuitKey -> ResolutionMsg mapping.
	resBucketKey = []byte("resolution-store-bucket-key")

	// errResMsgNotFound is used to let callers know that the resolution
	// message was not found for the given CircuitKey. This is used in the
	// checkResolutionMsg function.
	errResMsgNotFound = errors.New("resolution message not found")
)

// resolutionStore contains ResolutionMsgs received from the contractcourt. The
// Switch deletes these from the store when the underlying circuit has been
// removed via DeleteCircuits. If the circuit hasn't been deleted, the Switch
// will dispatch the ResolutionMsg to a link if this was a multi-hop HTLC or to
// itself if the Switch initiated the payment.
type resolutionStore struct {
	backend kvdb.Backend
}

func newResolutionStore(db kvdb.Backend) *resolutionStore {
	return &resolutionStore{
		backend: db,
	}
}

// addResolutionMsg persists a ResolutionMsg to the resolutionStore.
func (r *resolutionStore) addResolutionMsg(
	resMsg *contractcourt.ResolutionMsg) error {

	// The outKey will be the database key.
	outKey := &CircuitKey{
		ChanID: resMsg.SourceChan,
		HtlcID: resMsg.HtlcIndex,
	}

	var resBuf bytes.Buffer
	if err := serializeResolutionMsg(&resBuf, resMsg); err != nil {
		return err
	}

	err := kvdb.Update(r.backend, func(tx kvdb.RwTx) error {
		resBucket, err := tx.CreateTopLevelBucket(resBucketKey)
		if err != nil {
			return err
		}

		return resBucket.Put(outKey.Bytes(), resBuf.Bytes())
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// checkResolutionMsg returns nil if the resolution message is found in the
// store. It returns an error if no resolution message was found for the
// passed outKey or if a database error occurred.
func (r *resolutionStore) checkResolutionMsg(outKey *CircuitKey) error {
	err := kvdb.View(r.backend, func(tx kvdb.RTx) error {
		resBucket := tx.ReadBucket(resBucketKey)
		if resBucket == nil {
			// Return an error if the bucket doesn't exist.
			return errResMsgNotFound
		}

		msg := resBucket.Get(outKey.Bytes())
		if msg == nil {
			// Return the not found error since no message exists
			// for this CircuitKey.
			return errResMsgNotFound
		}

		// Return nil to indicate that the message was found.
		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// fetchAllResolutionMsg returns a slice of all stored ResolutionMsgs. This is
// used by the Switch on start-up.
func (r *resolutionStore) fetchAllResolutionMsg() (
	[]*contractcourt.ResolutionMsg, error) {

	var msgs []*contractcourt.ResolutionMsg

	err := kvdb.View(r.backend, func(tx kvdb.RTx) error {
		resBucket := tx.ReadBucket(resBucketKey)
		if resBucket == nil {
			return nil
		}

		return resBucket.ForEach(func(k, v []byte) error {
			kr := bytes.NewReader(k)
			outKey := &CircuitKey{}
			if err := outKey.Decode(kr); err != nil {
				return err
			}

			vr := bytes.NewReader(v)
			resMsg, err := deserializeResolutionMsg(vr)
			if err != nil {
				return err
			}

			// Set the CircuitKey values on the ResolutionMsg.
			resMsg.SourceChan = outKey.ChanID
			resMsg.HtlcIndex = outKey.HtlcID

			msgs = append(msgs, resMsg)
			return nil
		})
	}, func() {
		msgs = nil
	})
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

// deleteResolutionMsg removes a ResolutionMsg with the passed-in CircuitKey.
func (r *resolutionStore) deleteResolutionMsg(outKey *CircuitKey) error {
	err := kvdb.Update(r.backend, func(tx kvdb.RwTx) error {
		resBucket, err := tx.CreateTopLevelBucket(resBucketKey)
		if err != nil {
			return err
		}

		return resBucket.Delete(outKey.Bytes())
	}, func() {})
	return err
}

// serializeResolutionMsg writes part of a ResolutionMsg to the passed
// io.Writer.
func serializeResolutionMsg(w io.Writer,
	resMsg *contractcourt.ResolutionMsg) error {

	isFail := resMsg.Failure != nil

	if err := channeldb.WriteElement(w, isFail); err != nil {
		return err
	}

	// If this is a failure message, then we're done serializing.
	if isFail {
		return nil
	}

	// Else this is a settle message, and we need to write the preimage.
	return channeldb.WriteElement(w, *resMsg.PreImage)
}

// deserializeResolutionMsg reads part of a ResolutionMsg from the passed
// io.Reader.
func deserializeResolutionMsg(r io.Reader) (*contractcourt.ResolutionMsg,
	error) {

	resMsg := &contractcourt.ResolutionMsg{}
	var isFail bool

	if err := channeldb.ReadElements(r, &isFail); err != nil {
		return nil, err
	}

	// If a failure resolution msg was stored, set the Failure field.
	if isFail {
		failureMsg := &lnwire.FailPermanentChannelFailure{}
		resMsg.Failure = failureMsg
		return resMsg, nil
	}

	var preimage [32]byte
	resMsg.PreImage = &preimage

	// Else this is a settle resolution msg and we will read the preimage.
	if err := channeldb.ReadElement(r, resMsg.PreImage); err != nil {
		return nil, err
	}

	return resMsg, nil
}
