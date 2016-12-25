package btcwallet

import (
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/wire"
)

// GetBestBlock returns the current height and hash of the best known block
// within the main chain.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBestBlock() (*wire.ShaHash, int32, error) {
	return b.rpc.GetBestBlock()
}

// GetTxOut returns the original output referenced by the passed outpoint.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetUtxo(txid *wire.ShaHash, index uint32) (*wire.TxOut, error) {
	txout, err := b.rpc.GetTxOut(txid, index, false)
	if err != nil {
		return nil, err
	}

	pkScript, err := hex.DecodeString(txout.ScriptPubKey.Hex)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		// Sadly, gettxout returns the output value in BTC
		// instead of satoshis.
		Value:    int64(txout.Value * 1e8),
		PkScript: pkScript,
	}, nil
}

// GetTransaction returns the full transaction identified by the passed
// transaction ID.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetTransaction(txid *wire.ShaHash) (*wire.MsgTx, error) {
	tx, err := b.rpc.GetRawTransaction(txid)
	if err != nil {
		return nil, err
	}

	return tx.MsgTx(), nil
}

// GetBlock returns a raw block from the server given its hash.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlock(blockHash *wire.ShaHash) (*wire.MsgBlock, error) {
	block, err := b.rpc.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	return block, nil
}

// GetBlockHash returns the hash of the block in the best block chain at the
// given height.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlockHash(blockHeight int64) (*wire.ShaHash, error) {
	blockHash, err := b.rpc.GetBlockHash(blockHeight)
	if err != nil {
		return nil, err
	}

	return blockHash, nil
}

// A compile time check to ensure that BtcWallet implements the BlockChainIO
// interface.
var _ lnwallet.WalletController = (*BtcWallet)(nil)
