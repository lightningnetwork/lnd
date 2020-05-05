package lnrpc

import (
	"encoding/hex"
	"sort"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// RPCTransactionDetails returns a set of rpc transaction details.
func RPCTransactionDetails(txns []*lnwallet.TransactionDetail) *TransactionDetails {
	txDetails := &TransactionDetails{
		Transactions: make([]*Transaction, len(txns)),
	}

	for i, tx := range txns {
		var destAddresses []string
		for _, destAddress := range tx.DestAddresses {
			destAddresses = append(destAddresses, destAddress.EncodeAddress())
		}

		// We also get unconfirmed transactions, so BlockHash can be
		// nil.
		blockHash := ""
		if tx.BlockHash != nil {
			blockHash = tx.BlockHash.String()
		}

		txDetails.Transactions[i] = &Transaction{
			TxHash:           tx.Hash.String(),
			Amount:           int64(tx.Value),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        blockHash,
			BlockHeight:      tx.BlockHeight,
			TimeStamp:        tx.Timestamp,
			TotalFees:        tx.TotalFees,
			DestAddresses:    destAddresses,
			RawTxHex:         hex.EncodeToString(tx.RawTx),
		}
	}

	// Sort transactions by number of confirmations rather than height so
	// that unconfirmed transactions (height =0; confirmations =-1) will
	// follow the most recently set of confirmed transactions. If we sort
	// by height, unconfirmed transactions will follow our oldest
	// transactions, because they have lower block heights.
	sort.Slice(txDetails.Transactions, func(i, j int) bool {
		return txDetails.Transactions[i].NumConfirmations <
			txDetails.Transactions[j].NumConfirmations
	})

	return txDetails
}
