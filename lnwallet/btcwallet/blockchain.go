package btcwallet

import (
	"encoding/hex"
	"errors"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

var (
	// ErrOutputSpent is returned by the GetUtxo method if the target output
	// for lookup has already been spent.
	ErrOutputSpent = errors.New("target output has been spent")
)

// GetBestBlock returns the current height and hash of the best known block
// within the main chain.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBestBlock() (*chainhash.Hash, int32, error) {
	return b.rpc.GetBestBlock()
}

// GetTxOut returns the original output referenced by the passed outpoint.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetUtxo(txid *chainhash.Hash, index uint32) (*wire.TxOut, error) {
	txout, err := b.rpc.GetTxOut(txid, index, false)
	if err != nil {
		return nil, err
	} else if txout == nil {
		return nil, ErrOutputSpent
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
func (b *BtcWallet) GetTransaction(txid *chainhash.Hash) (*wire.MsgTx, error) {
	tx, err := b.rpc.GetRawTransaction(txid)
	if err != nil {
		return nil, err
	}

	return tx.MsgTx(), nil
}

// GetBlock returns a raw block from the server given its hash.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	block, err := b.rpc.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	return block, nil
}

// GetBlockHash returns the hash of the block in the best blockchain at the
// given height.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	blockHash, err := b.rpc.GetBlockHash(blockHeight)
	if err != nil {
		return nil, err
	}

	return blockHash, nil
}

// A compile time check to ensure that BtcWallet implements the BlockChainIO
// interface.
var _ lnwallet.WalletController = (*BtcWallet)(nil)
