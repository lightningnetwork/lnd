package lntest

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// Miner returns the miner instance.
//
// NOTE: Caller should keep in mind that when using this private instance,
// certain states won't be managed by the HarnessTest anymore. For instance,
// when mining directly, the nodes managed by the HarnessTest can be out of
// sync, and the `HarnessTest.CurrentHeight()` won't be accurate.
func (h *HarnessTest) Miner() *miner.HarnessMiner {
	return h.miner
}

// MineBlocks mines blocks and asserts all active nodes have synced to the
// chain. It assumes no txns are expected in the blocks.
//
// NOTE: Use `MineBlocksAndAssertNumTxes` if you expect txns in the blocks. Use
// `MineEmptyBlocks` if you want to make sure that txns stay unconfirmed.
func (h *HarnessTest) MineBlocks(num int) {
	require.Less(h, num, maxBlocksAllowed, "too many blocks to mine")

	// Update the harness's current height.
	defer h.updateCurrentHeight()

	// Mine num of blocks.
	for i := 0; i < num; i++ {
		block := h.miner.MineBlocks(1)[0]

		// Check the block doesn't have any txns except the coinbase.
		if len(block.Transactions) <= 1 {
			// Make sure all the active nodes are synced.
			h.AssertActiveNodesSyncedTo(block.BlockHash())

			// Mine the next block.
			continue
		}

		// Create a detailed description.
		desc := fmt.Sprintf("block %v has %d txns:\n",
			block.BlockHash(), len(block.Transactions)-1)

		// Print all the txns except the coinbase.
		for _, tx := range block.Transactions {
			if blockchain.IsCoinBaseTx(tx) {
				continue
			}

			desc += fmt.Sprintf("%v\n", tx.TxHash())
		}

		desc += "Consider using `MineBlocksAndAssertNumTxes` if you " +
			"expect txns, or `MineEmptyBlocks` if you want to " +
			"keep the txns unconfirmed."

		// Raise an error if the block has txns.
		require.Fail(h, "MineBlocks", desc)
	}
}

// MineEmptyBlocks mines a given number of empty blocks.
//
// NOTE: this differs from miner's `MineEmptyBlocks` as it requires the nodes
// to be synced.
func (h *HarnessTest) MineEmptyBlocks(num int) []*wire.MsgBlock {
	require.Less(h, num, maxBlocksAllowed, "too many blocks to mine")

	// Update the harness's current height.
	defer h.updateCurrentHeight()

	blocks := h.miner.MineEmptyBlocks(num)

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
func (h *HarnessTest) MineBlocksAndAssertNumTxes(num uint32,
	numTxs int) []*wire.MsgBlock {

	// Update the harness's current height.
	defer h.updateCurrentHeight()

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	txids := h.AssertNumTxsInMempool(numTxs)

	// Mine blocks.
	blocks := h.miner.MineBlocks(num)

	// Assert that all the transactions were included in the first block.
	for _, txid := range txids {
		h.miner.AssertTxInBlock(blocks[0], txid)
	}

	// Make sure the mempool has been updated.
	h.miner.AssertTxnsNotInMempool(txids)

	// Finally, make sure all the active nodes are synced.
	bestBlock := blocks[len(blocks)-1]
	h.AssertActiveNodesSyncedTo(bestBlock.BlockHash())

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
	_, startHeight := h.GetBestBlock()

	// Mining the blocks slow to give `lnd` more time to sync.
	var bestBlock *wire.MsgBlock
	err := wait.NoError(func() error {
		// If mempool is empty, exit.
		mem := h.miner.GetRawMempool()
		if len(mem) == 0 {
			_, height := h.GetBestBlock()
			h.Logf("Mined %d blocks when cleanup the mempool",
				height-startHeight)

			return nil
		}

		// Otherwise mine a block.
		blocks := h.miner.MineBlocksSlow(1)
		bestBlock = blocks[len(blocks)-1]

		// Make sure all the active nodes are synced.
		h.AssertActiveNodesSyncedTo(bestBlock.BlockHash())

		return fmt.Errorf("still have %d txes in mempool", len(mem))
	}, wait.MinerMempoolTimeout)
	require.NoError(h, err, "timeout cleaning up mempool")
}

// mineTillForceCloseResolved asserts that the number of pending close channels
// are zero. Each time it checks, an empty block is mined, followed by a
// mempool check to see if there are any sweeping txns. If found, these txns
// are then mined to clean up the mempool.
func (h *HarnessTest) mineTillForceCloseResolved(hn *node.HarnessNode) {
	_, startHeight := h.GetBestBlock()

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		total := len(resp.PendingForceClosingChannels)
		if total != 0 {
			// Mine an empty block first.
			h.MineEmptyBlocks(1)

			// If there are new sweeping txns, mine a block to
			// confirm it.
			mem := h.GetRawMempool()
			if len(mem) != 0 {
				h.MineBlocksAndAssertNumTxes(1, len(mem))
			}

			return fmt.Errorf("expected num of pending force " +
				"close channel to be zero")
		}

		_, height := h.GetBestBlock()
		h.Logf("Mined %d blocks while waiting for force closed "+
			"channel to be resolved", height-startHeight)

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: assert force close resolved timeout",
		hn.Name())
}

// AssertTxInMempool asserts a given transaction can be found in the mempool.
func (h *HarnessTest) AssertTxInMempool(txid chainhash.Hash) *wire.MsgTx {
	return h.miner.AssertTxInMempool(txid)
}

// AssertTxNotInMempool asserts a given transaction cannot be found in the
// mempool. It assumes the mempool is not empty.
//
// NOTE: this should be used after `AssertTxInMempool` to ensure the tx has
// entered the mempool before. Otherwise it might give false positive and the
// tx may enter the mempool after the check.
func (h *HarnessTest) AssertTxNotInMempool(txid chainhash.Hash) {
	h.miner.AssertTxNotInMempool(txid)
}

// AssertNumTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. It will assert if this number is not met
// after the given timeout.
func (h *HarnessTest) AssertNumTxsInMempool(n int) []chainhash.Hash {
	return h.miner.AssertNumTxsInMempool(n)
}

// AssertOutpointInMempool asserts a given outpoint can be found in the mempool.
func (h *HarnessTest) AssertOutpointInMempool(op wire.OutPoint) *wire.MsgTx {
	return h.miner.AssertOutpointInMempool(op)
}

// AssertTxInBlock asserts that a given txid can be found in the passed block.
func (h *HarnessTest) AssertTxInBlock(block *wire.MsgBlock,
	txid chainhash.Hash) {

	h.miner.AssertTxInBlock(block, txid)
}

// GetNumTxsFromMempool polls until finding the desired number of transactions
// in the miner's mempool and returns the full transactions to the caller.
func (h *HarnessTest) GetNumTxsFromMempool(n int) []*wire.MsgTx {
	return h.miner.GetNumTxsFromMempool(n)
}

// GetBestBlock makes a RPC request to miner and asserts.
func (h *HarnessTest) GetBestBlock() (*chainhash.Hash, int32) {
	return h.miner.GetBestBlock()
}

// MineBlockWithTx mines a single block to include the specifies tx only.
func (h *HarnessTest) MineBlockWithTx(tx *wire.MsgTx) *wire.MsgBlock {
	// Update the harness's current height.
	defer h.updateCurrentHeight()

	block := h.miner.MineBlockWithTx(tx)

	// Finally, make sure all the active nodes are synced.
	h.AssertActiveNodesSyncedTo(block.BlockHash())

	return block
}

// ConnectToMiner connects the miner to a temp miner.
func (h *HarnessTest) ConnectToMiner(tempMiner *miner.HarnessMiner) {
	h.miner.ConnectMiner(tempMiner)
}

// DisconnectFromMiner disconnects the miner from the temp miner.
func (h *HarnessTest) DisconnectFromMiner(tempMiner *miner.HarnessMiner) {
	h.miner.DisconnectMiner(tempMiner)
}

// GetRawMempool makes a RPC call to the miner's GetRawMempool and
// asserts.
func (h *HarnessTest) GetRawMempool() []chainhash.Hash {
	return h.miner.GetRawMempool()
}

// GetRawTransaction makes a RPC call to the miner's GetRawTransaction and
// asserts.
func (h *HarnessTest) GetRawTransaction(txid chainhash.Hash) *btcutil.Tx {
	return h.miner.GetRawTransaction(txid)
}

// NewMinerAddress creates a new address for the miner and asserts.
func (h *HarnessTest) NewMinerAddress() btcutil.Address {
	return h.miner.NewMinerAddress()
}

// SpawnTempMiner creates a temp miner and syncs it with the current miner.
// Once miners are synced, the temp miner is disconnected from the original
// miner and returned.
func (h *HarnessTest) SpawnTempMiner() *miner.HarnessMiner {
	return h.miner.SpawnTempMiner()
}

// CreateTransaction uses the miner to create a transaction using the given
// outputs using the specified fee rate and returns the transaction.
func (h *HarnessTest) CreateTransaction(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *wire.MsgTx {

	return h.miner.CreateTransaction(outputs, feeRate)
}

// SendOutputsWithoutChange uses the miner to send the given outputs using the
// specified fee rate and returns the txid.
func (h *HarnessTest) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *chainhash.Hash {

	return h.miner.SendOutputsWithoutChange(outputs, feeRate)
}

// AssertMinerBlockHeightDelta ensures that tempMiner is 'delta' blocks ahead
// of miner.
func (h *HarnessTest) AssertMinerBlockHeightDelta(
	tempMiner *miner.HarnessMiner, delta int32) {

	h.miner.AssertMinerBlockHeightDelta(tempMiner, delta)
}

// SendRawTransaction submits the encoded transaction to the server which will
// then relay it to the network.
func (h *HarnessTest) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (chainhash.Hash, error) {

	txid, err := h.miner.Client.SendRawTransaction(tx, allowHighFees)
	require.NoError(h, err)

	return *txid, nil
}

// CurrentHeight returns the current block height.
func (h *HarnessTest) CurrentHeight() uint32 {
	return h.currentHeight
}

// updateCurrentHeight set the harness's current height to the best known
// height.
func (h *HarnessTest) updateCurrentHeight() {
	_, height := h.GetBestBlock()
	h.currentHeight = uint32(height)
}
