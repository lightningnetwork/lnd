package channelcoord

import (
	"bytes"
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/linknode"
)

// KVCoordinator coordinates aggregate channel persistence for the KV backend.
// It owns operations that need a shared transaction across channel-state and
// link-node storage.
type KVCoordinator struct {
	backend        kvdb.Backend
	chanStateStore chanstate.Store

	// TODO: Depend on linknode.Store once link-node creation no longer
	// requires a concrete *linknode.LinkNodeDB to back LinkNode.Sync.
	linkNodeDB *linknode.LinkNodeDB
}

var _ Coordinator = (*KVCoordinator)(nil)

// NewKVCoordinator creates a new KV-backed channel coordinator.
func NewKVCoordinator(backend kvdb.Backend,
	linkNodeDB *linknode.LinkNodeDB,
	chanStateStore chanstate.Store) *KVCoordinator {

	return &KVCoordinator{
		backend:        backend,
		linkNodeDB:     linkNodeDB,
		chanStateStore: chanStateStore,
	}
}

// SyncPendingChannel writes a pending channel to the store and records the
// funding broadcast height.
func (c *KVCoordinator) SyncPendingChannel(channel *chanstate.OpenChannel,
	addr net.Addr, pendingHeight uint32) error {

	return c.syncPendingChannel(channel, addr, pendingHeight)
}

func (c *KVCoordinator) syncPendingChannel(channel *chanstate.OpenChannel,
	addr net.Addr, pendingHeight uint32) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		return c.syncNewChannelTx(
			tx, channel, []net.Addr{addr}, pendingHeight,
		)
	}, func() {})
}

// syncNewChannelTx writes the passed channel to disk, and also creates a
// LinkNode, if needed, for the channel peer.
func (c *KVCoordinator) syncNewChannelTx(tx kvdb.RwTx,
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
		c.linkNodeDB, wire.MainNet, channel.IdentityPub, addrs...,
	)

	// TODO(roasbeef): do away with link node all together?
	return linknode.PutLinkNode(nodeInfoBucket, linkNode)
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all
// the pending funds in a channel that has been forcibly closed have been
// swept.
func (c *KVCoordinator) MarkChanFullyClosed(
	chanPoint *wire.OutPoint) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
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

// pruneLinkNode determines whether we should garbage collect a link node from
// the database due to no longer having any open channels with it.
//
// NOTE: This function should be called after an initial check shows no open
// channels exist. It will double-check within a write transaction to avoid a
// race condition where a channel could be opened between the initial check and
// the deletion.
func (c *KVCoordinator) pruneLinkNode(remotePub *btcec.PublicKey) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		// Double-check for open channels to avoid deleting a link node
		// if a channel was opened since the caller's initial check.
		openChannels, err := chanstate.FetchOpenChannelsTx(
			tx, remotePub,
		)
		if err != nil {
			return err
		}

		if len(openChannels) > 0 {
			return nil
		}

		log.Infof("Pruning link node %x with zero open channels from "+
			"database", remotePub.SerializeCompressed())

		err = linknode.DeleteLinkNodeTx(tx, remotePub)
		if err != nil {
			return fmt.Errorf("unable to prune link node: %w", err)
		}

		return nil
	}, func() {})
}

// PruneLinkNodes attempts to prune all link nodes found within the database
// with whom we no longer have any open channels with.
func (c *KVCoordinator) PruneLinkNodes() error {
	allLinkNodes, err := c.linkNodeDB.FetchAllLinkNodes()
	if err != nil {
		return err
	}

	for _, linkNode := range allLinkNodes {
		var (
			openChannels []*chanstate.OpenChannel
			linkNode     = linkNode
		)
		err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
			var err error
			openChannels, err = chanstate.FetchOpenChannelsTx(
				tx, linkNode.IdentityPub,
			)

			return err
		}, func() {
			openChannels = nil
		})
		if err != nil {
			return err
		}

		if len(openChannels) > 0 {
			continue
		}

		err = c.pruneLinkNode(linkNode.IdentityPub)
		if err != nil {
			return err
		}
	}

	return nil
}

// RepairLinkNodes scans all channels in the database and ensures that a link
// node exists for each remote peer. This should be called on startup to ensure
// that our database is consistent.
//
// NOTE: This function is designed to repair database inconsistencies that may
// have occurred due to the race condition in link node pruning where link nodes
// could be incorrectly deleted while channels still existed. This can be
// removed once we move to native sql.
func (c *KVCoordinator) RepairLinkNodes(network wire.BitcoinNet) error {
	// In a single read transaction, build a list of all peers with open
	// channels and check which ones are missing link nodes.
	var missingPeers []*btcec.PublicKey

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		openChanBucket := chanstate.OpenChannelBucket(tx)
		if openChanBucket == nil {
			return chanstate.ErrNoActiveChannels
		}

		var peersWithChannels []*btcec.PublicKey

		err := openChanBucket.ForEach(func(nodePubBytes,
			_ []byte) error {

			nodePub, err := btcec.ParsePubKey(nodePubBytes)
			if err != nil {
				return err
			}

			channels, err := chanstate.FetchOpenChannelsTx(
				tx, nodePub,
			)
			if err != nil {
				return err
			}

			if len(channels) > 0 {
				peersWithChannels = append(
					peersWithChannels, nodePub,
				)
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Now check which peers are missing link nodes within the same
		// transaction.
		missingPeers, err = c.linkNodeDB.FindMissingLinkNodes(
			tx, peersWithChannels,
		)

		return err
	}, func() {
		missingPeers = nil
	})
	if err != nil && !errors.Is(err, chanstate.ErrNoActiveChannels) {
		return fmt.Errorf("unable to fetch channels: %w", err)
	}

	if len(missingPeers) == 0 {
		return nil
	}

	// Create all missing link nodes in a single write transaction using
	// the LinkNodeDB abstraction.
	linkNodesToCreate := make([]*linknode.LinkNode, 0, len(missingPeers))
	for _, remotePub := range missingPeers {
		linkNode := linknode.NewLinkNode(
			c.linkNodeDB, network, remotePub,
		)
		linkNodesToCreate = append(linkNodesToCreate, linkNode)

		log.Infof("Repairing missing link node for peer %x",
			remotePub.SerializeCompressed())
	}

	err = c.linkNodeDB.CreateLinkNodes(nil, linkNodesToCreate)
	if err != nil {
		return err
	}

	log.Infof("Repaired %d missing link nodes on startup",
		len(missingPeers))

	return nil
}

// RestoreChannelShells reconstructs the state of an OpenChannel from the
// ChannelShell. It writes the new channel to disk and creates the related
// LinkNode metadata. This method is idempotent, so repeated calls with the
// same set of channel shells won't modify the database after the initial call.
func (c *KVCoordinator) RestoreChannelShells(
	channelShells ...*chanstate.ChannelShell) error {

	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
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
			channel.Db = c.chanStateStore
			err := c.syncNewChannelTx(
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
