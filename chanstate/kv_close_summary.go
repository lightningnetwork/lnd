package chanstate

import (
	"bytes"
	"errors"
	"io"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("closed-chan-bucket")
)

// ClosedChannelBucketKey returns the top-level closed-channel summary bucket
// key.
func ClosedChannelBucketKey() []byte {
	return closedChannelBucket
}

// PutChannelCloseSummary writes the immutable close-time summary of a channel
// under the closed channel bucket.
func PutChannelCloseSummary(tx kvdb.RwTx, chanID []byte,
	summary *ChannelCloseSummary, lastChanState *OpenChannel) error {

	closedChanBucket, err := tx.CreateTopLevelBucket(closedChannelBucket)
	if err != nil {
		return err
	}

	summary.RemoteCurrentRevocation = lastChanState.RemoteCurrentRevocation
	summary.RemoteNextRevocation = lastChanState.RemoteNextRevocation
	summary.LocalChanConfig = lastChanState.LocalChanCfg

	var b bytes.Buffer
	if err := SerializeChannelCloseSummary(&b, summary); err != nil {
		return err
	}

	return closedChanBucket.Put(chanID, b.Bytes())
}

// CloseChannel closes the supplied channel via the selected close strategy. On
// synchronous backends the channel's nested state — the revocation log, the
// per-channel forwarding-package bucket, and the chanBucket itself — is deleted
// inline. On tombstone-enabled backends none of the bulk state is touched; the
// outpointBucket flip to outpointClosed signals that the channel is logically
// closed.
func CloseChannel(backend kvdb.Backend, channel *OpenChannel,
	summary *ChannelCloseSummary, tombstoneClosedChannels bool,
	statuses ...ChannelStatus) error {

	if tombstoneClosedChannels {
		return CloseChannelTombstone(
			backend, channel, summary, statuses...,
		)
	}

	return CloseChannelSync(backend, channel, summary, statuses...)
}

// LocateOpenChannel performs the open-channel-bucket descent for a CloseChannel
// transaction: it returns the chain bucket, the channel bucket, and the
// serialized chanKey for the supplied OpenChannel. A chanKey already flipped to
// outpointClosed surfaces ErrChannelNotFound so a redundant CloseChannel does
// not re-archive or re-flip the index.
func LocateOpenChannel(tx kvdb.RwTx, channel *OpenChannel) (kvdb.RwBucket,
	kvdb.RwBucket, []byte, error) {

	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, nil, nil, ErrNoChanDBExists
	}

	nodePub := channel.IdentityPub.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	chainBucket := nodeChanBucket.NestedReadWriteBucket(
		channel.ChainHash[:],
	)
	if chainBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(
		&chanPointBuf, &channel.FundingOutpoint,
	); err != nil {
		return nil, nil, nil, err
	}
	chanKey := chanPointBuf.Bytes()

	chanBucket := chainBucket.NestedReadWriteBucket(chanKey)
	if chanBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	// A channel whose outpoint is already flipped to outpointClosed must
	// not be re-closed: on tombstone backends the chanBucket survives a
	// previous close, but the index flip is the authoritative record that
	// the channel is gone from the open-channel view.
	closed, err := IsOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, nil, nil, err
	}
	if closed {
		return nil, nil, nil, ErrChannelNotFound
	}

	return chainBucket, chanBucket, chanKey, nil
}

// ArchiveClosedChannel writes the immutable close-time records of the channel:
// a copy of the open-channel state under historicalChannelBucket (with the
// supplied close statuses OR'd into chanStatus) and the close summary under
// closeSummaryBucket.
func ArchiveClosedChannel(tx kvdb.RwTx, chanKey []byte,
	chanState *OpenChannel, summary *ChannelCloseSummary,
	statuses ...ChannelStatus) error {

	historicalBucket, err := tx.CreateTopLevelBucket(
		historicalChannelBucket,
	)
	if err != nil {
		return err
	}
	historicalChanBucket, err := historicalBucket.CreateBucketIfNotExists(
		chanKey,
	)
	if err != nil {
		return err
	}

	for _, s := range statuses {
		chanState.SetChannelStatusForStore(
			chanState.ChannelStatusForStore() | s,
		)
	}

	if err := PutOpenChannel(historicalChanBucket, chanState); err != nil {
		return err
	}

	return PutChannelCloseSummary(tx, chanKey, summary, chanState)
}

// CloseChannelSync performs the historical synchronous close path: in a single
// write transaction it wipes the forwarding-package state, deletes the channel
// bucket and its nested revocation log entries, updates the outpoint index, and
// archives the close summary. It is used by backends where nested-bucket
// deletion is cheap (bbolt, etcd).
func CloseChannelSync(backend kvdb.Backend, channel *OpenChannel,
	summary *ChannelCloseSummary, statuses ...ChannelStatus) error {

	return kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chainBucket, chanBucket, chanKey, err := LocateOpenChannel(
			tx, channel,
		)
		if err != nil {
			return err
		}

		chanState, err := FetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		packager := NewChannelPackager(chanState.ShortChannelID)
		if err = packager.Wipe(tx); err != nil {
			return err
		}

		if err := DeleteOpenChannel(chanBucket); err != nil {
			return err
		}

		if channel.ChanType.IsFrozen() ||
			channel.ChanType.HasLeaseExpiration() {

			if err := DeleteThawHeight(chanBucket); err != nil {
				return err
			}
		}

		if err := DeleteLogBucket(chanBucket); err != nil {
			return err
		}

		if err := chainBucket.DeleteNestedBucket(chanKey); err != nil {
			return err
		}

		if err := UpdateClosedOutpointIndex(tx, chanKey); err != nil {
			return err
		}

		return ArchiveClosedChannel(
			tx, chanKey, chanState, summary, statuses...,
		)
	}, func() {})
}

// CloseChannelTombstone performs the tombstone close path used by KV-over-SQL
// backends. The channel's per-channel state is left intact — touching it would
// trigger the cascading nested-bucket delete this path exists to avoid — and
// the outpointBucket flip from outpointOpen to outpointClosed serves as the
// authoritative closed-channel marker. The disk space is reclaimed wholesale by
// the upcoming native-SQL channel-state migration.
func CloseChannelTombstone(backend kvdb.Backend, channel *OpenChannel,
	summary *ChannelCloseSummary, statuses ...ChannelStatus) error {

	return kvdb.Update(backend, func(tx kvdb.RwTx) error {
		_, chanBucket, chanKey, err := LocateOpenChannel(tx, channel)
		if err != nil {
			return err
		}

		chanState, err := FetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		if err := UpdateClosedOutpointIndex(tx, chanKey); err != nil {
			return err
		}

		return ArchiveClosedChannel(
			tx, chanKey, chanState, summary, statuses...,
		)
	}, func() {})
}

// SerializeChannelCloseSummary serializes a channel close summary.
func SerializeChannelCloseSummary(w io.Writer,
	cs *ChannelCloseSummary) error {

	err := WriteElements(w,
		cs.ChanPoint, cs.ShortChanID, cs.ChainHash, cs.ClosingTXID,
		cs.CloseHeight, cs.RemotePub, cs.Capacity, cs.SettledBalance,
		cs.TimeLockedBalance, cs.CloseType, cs.IsPending,
	)
	if err != nil {
		return err
	}

	// If this is a close channel summary created before the addition of
	// the new fields, then we can exit here.
	if cs.RemoteCurrentRevocation == nil {
		return WriteElements(w, false)
	}

	// If fields are present, write boolean to indicate this, and continue.
	if err := WriteElements(w, true); err != nil {
		return err
	}

	if err := WriteElements(w, cs.RemoteCurrentRevocation); err != nil {
		return err
	}

	if err := WriteChanConfig(w, &cs.LocalChanConfig); err != nil {
		return err
	}

	// The RemoteNextRevocation field is optional, as it's possible for a
	// channel to be closed before we learn of the next unrevoked
	// revocation point for the remote party. Write a boolean indicating
	// whether this field is present or not.
	if err := WriteElements(w, cs.RemoteNextRevocation != nil); err != nil {
		return err
	}

	// Write the field, if present.
	if cs.RemoteNextRevocation != nil {
		if err = WriteElements(w, cs.RemoteNextRevocation); err != nil {
			return err
		}
	}

	// Write whether the channel sync message is present.
	if err := WriteElements(w, cs.LastChanSyncMsg != nil); err != nil {
		return err
	}

	// Write the channel sync message, if present.
	if cs.LastChanSyncMsg != nil {
		if err := WriteElements(w, cs.LastChanSyncMsg); err != nil {
			return err
		}
	}

	return nil
}

// DeserializeCloseChannelSummary deserializes a channel close summary.
func DeserializeCloseChannelSummary(r io.Reader) (*ChannelCloseSummary, error) {
	c := &ChannelCloseSummary{}

	err := ReadElements(r,
		&c.ChanPoint, &c.ShortChanID, &c.ChainHash, &c.ClosingTXID,
		&c.CloseHeight, &c.RemotePub, &c.Capacity, &c.SettledBalance,
		&c.TimeLockedBalance, &c.CloseType, &c.IsPending,
	)
	if err != nil {
		return nil, err
	}

	// We'll now check to see if the channel close summary was encoded with
	// any of the additional optional fields.
	var hasNewFields bool
	err = ReadElements(r, &hasNewFields)
	if err != nil {
		return nil, err
	}

	// If fields are not present, we can return.
	if !hasNewFields {
		return c, nil
	}

	// Otherwise read the new fields.
	if err := ReadElements(r, &c.RemoteCurrentRevocation); err != nil {
		return nil, err
	}

	if err := ReadChanConfig(r, &c.LocalChanConfig); err != nil {
		return nil, err
	}

	// Finally, we'll attempt to read the next unrevoked commitment point
	// for the remote party. If we closed the channel before receiving a
	// channel_ready message then this might not be present. A boolean
	// indicating whether the field is present will come first.
	var hasRemoteNextRevocation bool
	err = ReadElements(r, &hasRemoteNextRevocation)
	if err != nil {
		return nil, err
	}

	// If this field was written, read it.
	if hasRemoteNextRevocation {
		err = ReadElements(r, &c.RemoteNextRevocation)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have a channel sync message to read.
	var hasChanSyncMsg bool
	err = ReadElements(r, &hasChanSyncMsg)
	if errors.Is(err, io.EOF) {
		return c, nil
	} else if err != nil {
		return nil, err
	}

	// If a chan sync message is present, read it.
	if hasChanSyncMsg {
		// We must pass in reference to a lnwire.Message for the codec
		// to support it.
		var msg lnwire.Message
		if err := ReadElements(r, &msg); err != nil {
			return nil, err
		}

		chanSync, ok := msg.(*lnwire.ChannelReestablish)
		if !ok {
			return nil, errors.New("unable cast db Message to " +
				"ChannelReestablish")
		}
		c.LastChanSyncMsg = chanSync
	}

	return c, nil
}
