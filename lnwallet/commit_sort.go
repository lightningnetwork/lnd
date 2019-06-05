package lnwallet

import (
	"bytes"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// InPlaceCommitSort performs an in-place sort of a commitment transaction,
// given an unsorted transaction and a list of CLTV values for the HTLCs.
//
// The sort applied is a modified BIP69 sort, that uses the CLTV values of HTLCs
// as a tie breaker in case two HTLC outputs have an identical amount and
// pkscript. The pkscripts can be the same if they share the same payment hash,
// but since the CLTV is enforced via the nLockTime of the second-layer
// transactions, the script does not directly commit to them. Instead, the CLTVs
// must be supplied separately to act as a tie-breaker, otherwise we may produce
// invalid HTLC signatures if the receiver produces an alternative ordering
// during verification.
//
// NOTE: Commitment outputs should have a 0 CLTV corresponding to their index on
// the unsorted commitment transaction.
func InPlaceCommitSort(tx *wire.MsgTx, cltvs []uint32) {
	if len(tx.TxOut) != len(cltvs) {
		panic("output and cltv list size mismatch")
	}

	sort.Sort(sortableInputSlice(tx.TxIn))
	sort.Sort(sortableCommitOutputSlice{tx.TxOut, cltvs})
}

// sortableInputSlice is a slice of transaction inputs that supports sorting via
// BIP69.
type sortableInputSlice []*wire.TxIn

// Len returns the length of the sortableInputSlice.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableInputSlice) Len() int { return len(s) }

// Swap exchanges the position of inputs i and j.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableInputSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Less is the BIP69 input comparison function. The sort is first applied on
// input hash (reversed / rpc-style), then index. This logic is copied from
// btcutil/txsort.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableInputSlice) Less(i, j int) bool {
	// Input hashes are the same, so compare the index.
	ihash := s[i].PreviousOutPoint.Hash
	jhash := s[j].PreviousOutPoint.Hash
	if ihash == jhash {
		return s[i].PreviousOutPoint.Index < s[j].PreviousOutPoint.Index
	}

	// At this point, the hashes are not equal, so reverse them to
	// big-endian and return the result of the comparison.
	const hashSize = chainhash.HashSize
	for b := 0; b < hashSize/2; b++ {
		ihash[b], ihash[hashSize-1-b] = ihash[hashSize-1-b], ihash[b]
		jhash[b], jhash[hashSize-1-b] = jhash[hashSize-1-b], jhash[b]
	}
	return bytes.Compare(ihash[:], jhash[:]) == -1
}

// sortableCommitOutputSlice is a slice of transaction outputs on a commitment
// transaction and the corresponding CLTV values of any HTLCs. Commitment
// outputs should have a CLTV of 0 and the same index in cltvs.
type sortableCommitOutputSlice struct {
	txouts []*wire.TxOut
	cltvs  []uint32
}

// Len returns the length of the sortableCommitOutputSlice.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableCommitOutputSlice) Len() int {
	return len(s.txouts)
}

// Swap exchanges the position of outputs i and j, as well as their
// corresponding CLTV values.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableCommitOutputSlice) Swap(i, j int) {
	s.txouts[i], s.txouts[j] = s.txouts[j], s.txouts[i]
	s.cltvs[i], s.cltvs[j] = s.cltvs[j], s.cltvs[i]
}

// Less is a modified BIP69 output comparison, that sorts based on value, then
// pkscript, then CLTV value.
//
// NOTE: Part of the sort.Interface interface.
func (s sortableCommitOutputSlice) Less(i, j int) bool {
	outi, outj := s.txouts[i], s.txouts[j]

	if outi.Value != outj.Value {
		return outi.Value < outj.Value
	}

	pkScriptCmp := bytes.Compare(outi.PkScript, outj.PkScript)
	if pkScriptCmp != 0 {
		return pkScriptCmp < 0
	}

	return s.cltvs[i] < s.cltvs[j]
}
