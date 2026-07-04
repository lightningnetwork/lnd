package chanlifecycle

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/linknode"
)

// KVLinkNodeMaintainer implements link-node maintenance derived from channel
// state for the KV backend.
type KVLinkNodeMaintainer struct {
	backend    kvdb.Backend
	linkNodeDB linknode.Store
}

var _ LinkNodeMaintainer = (*KVLinkNodeMaintainer)(nil)

// NewKVLinkNodeMaintainer creates a KV-backed link-node maintainer.
func NewKVLinkNodeMaintainer(backend kvdb.Backend,
	linkNodeDB linknode.Store) *KVLinkNodeMaintainer {

	return &KVLinkNodeMaintainer{
		backend:    backend,
		linkNodeDB: linkNodeDB,
	}
}

// pruneLinkNode determines whether we should garbage collect a link node from
// the database due to no longer having any open channels with it.
//
// NOTE: This function should be called after an initial check shows no open
// channels exist. It will double-check within a write transaction to avoid a
// race condition where a channel could be opened between the initial check and
// the deletion.
func (k *KVLinkNodeMaintainer) pruneLinkNode(
	remotePub *btcec.PublicKey) error {

	return kvdb.Update(k.backend, func(tx kvdb.RwTx) error {
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
func (k *KVLinkNodeMaintainer) PruneLinkNodes() error {
	allLinkNodes, err := k.linkNodeDB.FetchAllLinkNodes()
	if err != nil {
		return err
	}

	for _, linkNode := range allLinkNodes {
		var (
			openChannels []*chanstate.OpenChannel
			linkNode     = linkNode
		)
		err := kvdb.View(k.backend, func(tx kvdb.RTx) error {
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

		err = k.pruneLinkNode(linkNode.IdentityPub)
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
func (k *KVLinkNodeMaintainer) RepairLinkNodes(
	network wire.BitcoinNet) error {

	// In a single read transaction, build a list of all peers with open
	// channels and check which ones are missing link nodes.
	var missingPeers []*btcec.PublicKey

	err := kvdb.View(k.backend, func(tx kvdb.RTx) error {
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
		missingPeers, err = linknode.FindMissingLinkNodesTx(
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
			k.linkNodeDB, network, remotePub,
		)
		linkNodesToCreate = append(linkNodesToCreate, linkNode)

		log.Infof("Repairing missing link node for peer %x",
			remotePub.SerializeCompressed())
	}

	err = k.linkNodeDB.CreateLinkNodes(linkNodesToCreate)
	if err != nil {
		return err
	}

	log.Infof("Repaired %d missing link nodes on startup",
		len(missingPeers))

	return nil
}
