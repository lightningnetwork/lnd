package lnrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestRPCTransactionDetailsReverse checks that RPCTransactionDetails orders the
// returned transactions newest to oldest by default, and oldest to newest when
// reverse is set. Ordering is keyed off the number of confirmations so that
// unconfirmed transactions stay adjacent to the most recent confirmed ones.
func TestRPCTransactionDetailsReverse(t *testing.T) {
	t.Parallel()

	// Construct transactions across three block heights plus one
	// unconfirmed transaction. A more recent transaction has fewer
	// confirmations, and an unconfirmed transaction has zero.
	txns := []*lnwallet.TransactionDetail{
		{BlockHeight: 401, NumConfirmations: 3},
		{BlockHeight: 402, NumConfirmations: 2},
		{BlockHeight: 403, NumConfirmations: 1},
		{BlockHeight: 0, NumConfirmations: 0},
	}

	// Default ordering is newest to oldest: the unconfirmed transaction
	// first, then descending block height.
	forward := RPCTransactionDetails(txns, 0, 0, false)
	forwardHeights := make([]int32, len(forward.Transactions))
	for i, tx := range forward.Transactions {
		forwardHeights[i] = tx.BlockHeight
	}
	require.Equal(t, []int32{0, 403, 402, 401}, forwardHeights)

	// With reverse set the ordering flips to oldest to newest, ascending
	// block height, with the unconfirmed transaction last.
	reverse := RPCTransactionDetails(txns, 0, 0, true)
	reverseHeights := make([]int32, len(reverse.Transactions))
	for i, tx := range reverse.Transactions {
		reverseHeights[i] = tx.BlockHeight
	}
	require.Equal(t, []int32{401, 402, 403, 0}, reverseHeights)
}
