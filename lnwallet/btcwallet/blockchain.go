package btcwallet

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"

	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/waddrmgr"
)

var (
	// ErrOutputSpent is returned by the GetUtxo method if the target output
	// for lookup has already been spent.
	ErrOutputSpent = errors.New("target output has been spent")

	// ErrOutputNotFound signals that the desired output could not be
	// located.
	ErrOutputNotFound = errors.New("target output was not found")
)

// GetBestBlock returns the current height and hash of the best known block
// within the main chain.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBestBlock() (*chainhash.Hash, int32, error) {
	return b.chain.GetBestBlock()
}

// GetUtxo returns the original output referenced by the passed outpoint.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetUtxo(op *wire.OutPoint, heightHint uint32) (*wire.TxOut, error) {
	switch backend := b.chain.(type) {

	case *chain.NeutrinoClient:
		spendReport, err := backend.CS.GetUtxo(
			neutrino.WatchOutPoints(*op),
			neutrino.StartBlock(&waddrmgr.BlockStamp{
				Height: int32(heightHint),
			}),
		)
		if err != nil {
			return nil, err
		}

		// If the spend report is nil, then the output was not found in
		// the rescan.
		if spendReport == nil {
			return nil, ErrOutputNotFound
		}

		// If the spending transaction is populated in the spend report,
		// this signals that the output has already been spent.
		if spendReport.SpendingTx != nil {
			return nil, ErrOutputSpent
		}

		// Otherwise, the output is assumed to be in the UTXO.
		return spendReport.Output, nil

	case *chain.RPCClient:
		txout, err := backend.GetTxOut(&op.Hash, op.Index, false)
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

	case *chain.BitcoindClient:
		txout, err := backend.GetTxOut(&op.Hash, op.Index, false)
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

	default:
		return nil, fmt.Errorf("unknown backend")
	}
}

// GetBlock returns a raw block from the server given its hash.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return b.chain.GetBlock(blockHash)
}

// GetBlockHash returns the hash of the block in the best blockchain at the
// given height.
//
// This method is a part of the lnwallet.BlockChainIO interface.
func (b *BtcWallet) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return b.chain.GetBlockHash(blockHeight)
}

// A compile time check to ensure that BtcWallet implements the BlockChainIO
// interface.
var _ lnwallet.WalletController = (*BtcWallet)(nil)
