package btcdnotify

import (
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

// ChainConnection...
// Required in order to avoid an import cycle, and do aide in testing.
type ChainConnection interface {
	ListenConnectedBlocks() (<-chan wtxmgr.BlockMeta, error)
	ListenDisconnectedBlocks() (<-chan wtxmgr.BlockMeta, error)
	ListenRelevantTxs() (<-chan chain.RelevantTx, error)
}
