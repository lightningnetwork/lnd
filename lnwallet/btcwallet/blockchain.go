package btcwallet

import (
	"encoding/hex"

	"github.com/roasbeef/btcd/wire"
)

// GetCurrentHeight returns the current height of the known block within the
// main chain.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetCurrentHeight() (int32, error) {
	_, height, err := b.rpc.GetBestBlock()
	if err != nil {
		return 0, err
	}

	return height, nil
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
		Value:    int64(txout.Value) * 1e8,
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
