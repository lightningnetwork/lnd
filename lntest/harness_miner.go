package lntest

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// MineBlocks mines blocks and asserts all active nodes have synced to the
// chain.
//
// NOTE: this differs from miner's `MineBlocks` as it requires the nodes to be
// synced.
func (h *HarnessTest) MineBlocks(num uint32) []*wire.MsgBlock {
	require.Less(h, num, uint32(maxBlocksAllowed),
		"too many blocks to mine")

	// Mining the blocks slow to give `lnd` more time to sync.
	blocks := h.Miner.MineBlocksSlow(num)

	// Make sure all the active nodes are synced.
	bestBlock := blocks[len(blocks)-1]
	h.AssertActiveNodesSyncedTo(bestBlock)

	return blocks
}

// MineEmptyBlocks mines a given number of empty blocks.
//
// NOTE: this differs from miner's `MineEmptyBlocks` as it requires the nodes
// to be synced.
func (h *HarnessTest) MineEmptyBlocks(num int) []*wire.MsgBlock {
	require.Less(h, num, maxBlocksAllowed, "too many blocks to mine")

	blocks := h.Miner.MineEmptyBlocks(num)

	// Finally, make sure all the active nodes are synced.
	h.AssertActiveNodesSynced()

	return blocks
}

// MineBlocksAndAssertNumTxes mines blocks and asserts the number of
// transactions are found in the first block. It also asserts all active nodes
// have synced to the chain.
//
// NOTE: this differs from miner's `MineBlocks` as it requires the nodes to be
// synced.
//
// TODO(yy): change the APIs to force callers to think about blocks and txns:
// - MineBlocksAndAssertNumTxes -> MineBlocks
// - add more APIs to mine a single tx.
func (h *HarnessTest) MineBlocksAndAssertNumTxes(num uint32,
	numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	txids := h.Miner.AssertNumTxsInMempool(numTxs)

	// Mine blocks.
	blocks := h.Miner.MineBlocksSlow(num)

	// Assert that all the transactions were included in the first block.
	for _, txid := range txids {
		h.Miner.AssertTxInBlock(blocks[0], txid)
	}

	// Make sure the mempool has been updated.
	for _, txid := range txids {
		h.Miner.AssertTxNotInMempool(*txid)
	}

	// Finally, make sure all the active nodes are synced.
	bestBlock := blocks[len(blocks)-1]
	h.AssertActiveNodesSyncedTo(bestBlock)

	return blocks
}

// ConnectMiner connects the miner with the chain backend in the network.
func (h *HarnessTest) ConnectMiner() {
	err := h.manager.chainBackend.ConnectMiner()
	require.NoError(h, err, "failed to connect miner")
}

// DisconnectMiner removes the connection between the miner and the chain
// backend in the network.
func (h *HarnessTest) DisconnectMiner() {
	err := h.manager.chainBackend.DisconnectMiner()
	require.NoError(h, err, "failed to disconnect miner")
}

// cleanMempool mines blocks till the mempool is empty and asserts all active
// nodes have synced to the chain.
func (h *HarnessTest) cleanMempool() {
	_, startHeight := h.Miner.GetBestBlock()

	// Mining the blocks slow to give `lnd` more time to sync.
	var bestBlock *wire.MsgBlock
	err := wait.NoError(func() error {
		// If mempool is empty, exit.
		mem := h.Miner.GetRawMempool()
		if len(mem) == 0 {
			_, height := h.Miner.GetBestBlock()
			h.Logf("Mined %d blocks when cleanup the mempool",
				height-startHeight)

			return nil
		}

		// Otherwise mine a block.
		blocks := h.Miner.MineBlocksSlow(1)
		bestBlock = blocks[len(blocks)-1]

		// Make sure all the active nodes are synced.
		h.AssertActiveNodesSyncedTo(bestBlock)

		return fmt.Errorf("still have %d txes in mempool", len(mem))
	}, wait.MinerMempoolTimeout)
	require.NoError(h, err, "timeout cleaning up mempool")
}

// mineTillForceCloseResolved asserts that the number of pending close channels
// are zero. Each time it checks, a new block is mined using MineBlocksSlow to
// give the node some time to catch up the chain.
//
// NOTE: this method is a workaround to make sure we have a clean mempool at
// the end of a channel force closure. We cannot directly mine blocks and
// assert channels being fully closed because the subsystems in lnd don't share
// the same block height. This is especially the case when blocks are produced
// too fast.
// TODO(yy): remove this workaround when syncing blocks are unified in all the
// subsystems.
func (h *HarnessTest) mineTillForceCloseResolved(hn *node.HarnessNode) {
	_, startHeight := h.Miner.GetBestBlock()

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		total := len(resp.PendingForceClosingChannels)
		if total != 0 {
			h.MineBlocks(1)

			return fmt.Errorf("expected num of pending force " +
				"close channel to be zero")
		}

		_, height := h.Miner.GetBestBlock()
		h.Logf("Mined %d blocks while waiting for force closed "+
			"channel to be resolved", height-startHeight)

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "assert force close resolved timeout")
}
