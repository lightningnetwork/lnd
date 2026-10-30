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
)

func deleteLinkNode(tx kvdb.RwTx, identity *btcec.PublicKey) error {
	return linknode.DeleteLinkNodeTx(tx, identity)
}
