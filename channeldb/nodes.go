package channeldb

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/linknode"
)

var (
	// nodeInfoBucket stores metadata pertaining to nodes that we've had
	// direct channel-based correspondence with. This bucket allows one to
	// query for all open channels pertaining to the node by exploring each
	// node's sub-bucket within the openChanBucket.
	nodeInfoBucket = linknode.NodeInfoBucket

	// NewLinkNode creates a new LinkNode from the provided parameters,
	// which is backed by an instance of a link node DB.
	NewLinkNode = linknode.NewLinkNode

	// NewLinkNodeDB creates a new link node database.
	NewLinkNodeDB = linknode.NewDB
)

// LinkNode stores metadata related to node's that we have/had a direct
// channel open with.
type LinkNode = linknode.LinkNode

// LinkNodeDB is a database that keeps track of all link nodes.
type LinkNodeDB = linknode.LinkNodeDB

// putLinkNode serializes then writes the encoded version of the passed link
// node into the nodeMetaBucket. This function is provided in order to allow
// the ability to re-use a database transaction across many operations.
func putLinkNode(nodeMetaBucket kvdb.RwBucket, l *LinkNode) error {
	return linknode.PutLinkNode(nodeMetaBucket, l)
}

func deleteLinkNode(tx kvdb.RwTx, identity *btcec.PublicKey) error {
	return linknode.DeleteLinkNodeTx(tx, identity)
}
