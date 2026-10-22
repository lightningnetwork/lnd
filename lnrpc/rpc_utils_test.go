package lnrpc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// chainHeight is the height of the current chain tip used by the ordering
// tests.
const chainHeight = 110

// txHash returns a 64 character hex transaction hash with the given two
// character hex prefix, so transactions are easy to tell apart in test
// failures.
func txHash(prefix string) string {
	return strings.Repeat(prefix, 32)
}

// blockHash returns a deterministic block hash for the given block height,
// so transactions in the same block share the same block hash.
func blockHash(t *testing.T, height int32) *chainhash.Hash {
	h, err := chainhash.NewHashFromStr(
		strings.Repeat(fmt.Sprintf("%02x", height), 32),
	)
	require.NoError(t, err)

	return h
}

// newTxDetail returns a transaction detail for a transaction with the given
// hash and number of confirmations. Unconfirmed transactions report zero
// confirmations, confirmed ones are placed in a block relative to the chain
// tip.
func newTxDetail(t *testing.T, hash string,
	confs int32) *lnwallet.TransactionDetail {

	h, err := chainhash.NewHashFromStr(hash)
	require.NoError(t, err)

	detail := &lnwallet.TransactionDetail{
		Hash:              *h,
		NumConfirmations:  confs,
		OutputDetails:     []lnwallet.OutputDetail{},
		PreviousOutpoints: []lnwallet.PreviousOutPoint{},
	}

	if confs > 0 {
		detail.BlockHeight = chainHeight - confs + 1
		detail.BlockHash = blockHash(t, detail.BlockHeight)
	}

	return detail
}

// txHashes returns the hashes of the response transactions in the order
// they appear in.
func txHashes(resp *TransactionDetails) []string {
	hashes := make([]string, 0, len(resp.Transactions))
	for _, tx := range resp.Transactions {
		hashes = append(hashes, tx.TxHash)
	}

	return hashes
}

// TestRPCTransactionDetailsOrdering asserts the exact ordering of the
// transactions returned by RPCTransactionDetails, for both the default and
// the reverse ordering.
//
// The default ordering lists unconfirmed transactions first, followed by the
// most recent confirmed transactions down to the oldest. The reverse ordering
// lists the oldest confirmed transactions first, up to the most recent one,
// with unconfirmed transactions last. Transactions sharing the same number of
// confirmations (e.g. transactions in the same block, or unconfirmed
// transactions) keep their wallet order in both orderings, so the order
// within a returned page is deterministic and the reverse ordering is not an
// exact mirror of the default one for such ties.
//
// The first and last index of the page are passed through unmodified in both
// orderings, as pagination is applied by the wallet layer before
// RPCTransactionDetails is invoked and is independent of the ordering.
func TestRPCTransactionDetailsOrdering(t *testing.T) {
	t.Parallel()

	// The fixture mimics the ordering produced by the wallet layer:
	// confirmed transactions from the oldest block to the newest, with
	// unconfirmed transactions last.
	//
	// It is deliberately tie-heavy and larger than the threshold below
	// which Go's unstable sort.Slice happens to be stable (it falls back
	// to insertion sort for 12 or fewer elements), so that an unstable
	// sort would reorder the ties and fail the ordering assertions.
	txns := []*lnwallet.TransactionDetail{
		// Oldest block, four transactions.
		newTxDetail(t, txHash("a1"), 10),
		newTxDetail(t, txHash("a2"), 10),
		newTxDetail(t, txHash("a3"), 10),
		newTxDetail(t, txHash("a4"), 10),

		// A single transaction in a later block.
		newTxDetail(t, txHash("b1"), 7),

		// Another block with four transactions.
		newTxDetail(t, txHash("c1"), 5),
		newTxDetail(t, txHash("c2"), 5),
		newTxDetail(t, txHash("c3"), 5),
		newTxDetail(t, txHash("c4"), 5),

		// Tip block, four transactions.
		newTxDetail(t, txHash("d1"), 1),
		newTxDetail(t, txHash("d2"), 1),
		newTxDetail(t, txHash("d3"), 1),
		newTxDetail(t, txHash("d4"), 1),

		// Unconfirmed transactions.
		newTxDetail(t, txHash("e1"), 0),
		newTxDetail(t, txHash("e2"), 0),
		newTxDetail(t, txHash("e3"), 0),
		newTxDetail(t, txHash("e4"), 0),
	}

	// Mimic a page cut by the wallet layer with a nonzero index offset to
	// assert the indexes are passed through unmodified.
	const (
		firstIdx = 3
		lastIdx  = firstIdx + 16
	)
	require.Len(t, txns, lastIdx-firstIdx+1)

	// The default ordering lists unconfirmed transactions first, then the
	// most recently confirmed transactions down to the oldest.
	// Transactions sharing a block, and the unconfirmed transactions,
	// keep their wallet order.
	defaultOrder := []string{
		txHash("e1"), txHash("e2"), txHash("e3"), txHash("e4"),
		txHash("d1"), txHash("d2"), txHash("d3"), txHash("d4"),
		txHash("c1"), txHash("c2"), txHash("c3"), txHash("c4"),
		txHash("b1"),
		txHash("a1"), txHash("a2"), txHash("a3"), txHash("a4"),
	}

	resp := RPCTransactionDetails(txns, firstIdx, lastIdx, false)
	require.Equal(t, defaultOrder, txHashes(resp))
	require.EqualValues(t, firstIdx, resp.FirstIndex)
	require.EqualValues(t, lastIdx, resp.LastIndex)

	// The reverse ordering lists the oldest confirmed transactions first,
	// up to the most recent one, with unconfirmed transactions last. Ties
	// keep their wallet order here as well, so e.g. a1 still precedes a2.
	reverseOrder := []string{
		txHash("a1"), txHash("a2"), txHash("a3"), txHash("a4"),
		txHash("b1"),
		txHash("c1"), txHash("c2"), txHash("c3"), txHash("c4"),
		txHash("d1"), txHash("d2"), txHash("d3"), txHash("d4"),
		txHash("e1"), txHash("e2"), txHash("e3"), txHash("e4"),
	}

	resp = RPCTransactionDetails(txns, firstIdx, lastIdx, true)
	require.Equal(t, reverseOrder, txHashes(resp))
	require.EqualValues(t, firstIdx, resp.FirstIndex)
	require.EqualValues(t, lastIdx, resp.LastIndex)
}
