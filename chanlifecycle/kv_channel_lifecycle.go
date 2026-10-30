package chanlifecycle

import (
	"bytes"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/linknode"
)

// KVChannelLifecycle implements cross-store channel lifecycle operations for
// the KV backend. Each operation updates channel state and link-node metadata
// within a shared transaction.
type KVChannelLifecycle struct {
	backend        kvdb.Backend
	chanStateStore chanstate.Store
	linkNodeDB     linknode.Store
}

var _ ChannelLifecycle = (*KVChannelLifecycle)(nil)

// NewKVChannelLifecycle creates a KV-backed channel lifecycle implementation.
func NewKVChannelLifecycle(backend kvdb.Backend,
	linkNodeDB linknode.Store,
	chanStateStore chanstate.Store) *KVChannelLifecycle {

	return &KVChannelLifecycle{
		backend:        backend,
		linkNodeDB:     linkNodeDB,
		chanStateStore: chanStateStore,
	}
}

// SyncPendingChannel writes a pending channel to the store and records the
// funding broadcast height.
func (k *KVChannelLifecycle) SyncPendingChannel(
	channel *chanstate.OpenChannel, addr net.Addr,
	pendingHeight uint32) error {

	return kvdb.Update(k.backend, func(tx kvdb.RwTx) error {
		return k.syncNewChannelTx(
			tx, channel, []net.Addr{addr}, pendingHeight,
		)
	}, func() {})
}

// syncNewChannelTx writes the passed channel to disk, and also creates a
// LinkNode, if needed, for the channel peer.
func (k *KVChannelLifecycle) syncNewChannelTx(tx kvdb.RwTx,
	channel *chanstate.OpenChannel, addrs []net.Addr,
	pendingHeight uint32) error {

	// First, sync all the persistent channel state to disk.
	err := chanstate.SyncPendingOpenChannel(tx, channel, pendingHeight)
	if err != nil {
		return err
	}

	nodeInfoBucket, err := tx.CreateTopLevelBucket(linknode.NodeInfoBucket)
	if err != nil {
		return err
	}

	// If a LinkNode for this identity public key already exists, then we
	// can exit early.
	nodePub := channel.IdentityPub.SerializeCompressed()
	if nodeInfoBucket.Get(nodePub) != nil {
		return nil
	}

	// Next, we need to establish a possibly new LinkNode relationship for
	// this channel. The LinkNode metadata contains reachability, up-time,
	// and service bits related information.
	linkNode := linknode.NewLinkNode(
		k.linkNodeDB, wire.MainNet, channel.IdentityPub, addrs...,
	)

	// TODO(roasbeef): do away with link node all together?
	return linknode.PutLinkNode(nodeInfoBucket, linkNode)
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all
// the pending funds in a channel that has been forcibly closed have been
// swept.
func (k *KVChannelLifecycle) MarkChanFullyClosed(
	chanPoint *wire.OutPoint) error {

	return kvdb.Update(k.backend, func(tx kvdb.RwTx) error {
		var b bytes.Buffer
		if err := graphdb.WriteOutpoint(&b, chanPoint); err != nil {
			return err
		}

		chanID := b.Bytes()

		closedChanBucket, err := chanstate.ClosedChannelBucket(tx)
		if err != nil {
			return err
		}

		chanSummaryBytes := closedChanBucket.Get(chanID)
		if chanSummaryBytes == nil {
			return fmt.Errorf("no closed channel for "+
				"chan_point=%v found", chanPoint)
		}

		chanSummaryReader := bytes.NewReader(chanSummaryBytes)
		chanSummary, err := chanstate.DeserializeCloseChannelSummary(
			chanSummaryReader,
		)
		if err != nil {
			return err
		}

		chanSummary.IsPending = false

		var newSummary bytes.Buffer
		err = chanstate.SerializeChannelCloseSummary(
			&newSummary, chanSummary,
		)
		if err != nil {
			return err
		}

		err = closedChanBucket.Put(chanID, newSummary.Bytes())
		if err != nil {
			return err
		}

		// Now that the channel is closed, we'll check if we have
		// any other open channels with this peer. If we don't,
		// we'll garbage collect it to ensure we don't establish
		// persistent connections to peers without open channels.
		remotePub := chanSummary.RemotePub
		openChannels, err := chanstate.FetchOpenChannelsTx(
			tx, remotePub,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch open channels for "+
				"peer %x: %v",
				remotePub.SerializeCompressed(), err)
		}

		if len(openChannels) > 0 {
			return nil
		}

		// If there are no open channels with this peer, prune the link
		// node. We do this within the same transaction to avoid a race
		// condition where a new channel could be opened between this
		// check and the deletion.
		log.Infof("Pruning link node %x with zero open channels from "+
			"database", remotePub.SerializeCompressed())

		err = linknode.DeleteLinkNodeTx(tx, remotePub)
		if err != nil {
			return fmt.Errorf("unable to delete link node: %w", err)
		}

		return nil
	}, func() {})
}

// RestoreChannelShells reconstructs the state of an OpenChannel from the
// ChannelShell. It writes the new channel to disk and creates the related
// LinkNode metadata. This method is idempotent, so repeated calls with the
// same set of channel shells won't modify the database after the initial call.
func (k *KVChannelLifecycle) RestoreChannelShells(
	channelShells ...*chanstate.ChannelShell) error {

	err := kvdb.Update(k.backend, func(tx kvdb.RwTx) error {
		for _, channelShell := range channelShells {
			channel := channelShell.Chan

			// When we make a channel, we mark that the channel has
			// been restored, this will signal to other sub-systems
			// to not attempt to use the channel as if it was a
			// regular one.
			channel.SetChannelStatusForStore(
				channel.ChannelStatusForStore() |
					chanstate.ChanStatusRestored,
			)

			// First, we'll attempt to create a new open channel and
			// link node for this channel. If the channel already
			// exists, then in order to ensure this method is
			// idempotent, we'll continue to the next step.
			channel.Db = k.chanStateStore
			err := k.syncNewChannelTx(
				tx, channel, channelShell.NodeAddrs,
				channel.FundingBroadcastHeight,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}
