package lnwallet_test

import (
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type commitSortTest struct {
	name     string
	tx       *wire.MsgTx
	cltvs    []uint32
	intrans  map[int]int // input transformation
	outtrans map[int]int // output transformation
}

func (t *commitSortTest) expTxIns() []*wire.TxIn {
	if len(t.intrans) == 0 {
		return nil
	}

	expTxIns := make([]*wire.TxIn, len(t.intrans))
	for start, end := range t.intrans {
		expTxIns[end] = t.tx.TxIn[start]
	}

	return expTxIns
}

func (t *commitSortTest) expTxOuts() []*wire.TxOut {
	if len(t.outtrans) == 0 {
		return nil
	}

	expTxOuts := make([]*wire.TxOut, len(t.outtrans))
	for start, end := range t.outtrans {
		expTxOuts[end] = t.tx.TxOut[start]
	}

	return expTxOuts
}

var commitSortTests = []commitSortTest{
	{
		name: "sort inputs on prevoutpoint txid",
		tx: &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash: [32]byte{0x01},
					},
				},
				{
					PreviousOutPoint: wire.OutPoint{
						Hash: [32]byte{0x00},
					},
				},
			},
		},
		intrans: map[int]int{
			0: 1,
			1: 0,
		},
	},
	{
		name: "sort inputs on prevoutpoint index",
		tx: &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Index: 1,
					},
				},
				{
					PreviousOutPoint: wire.OutPoint{
						Index: 0,
					},
				},
			},
		},
		intrans: map[int]int{
			0: 1,
			1: 0,
		},
	},
	{
		name: "inputs already sorted",
		tx: &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Index: 0,
					},
				},
				{
					PreviousOutPoint: wire.OutPoint{
						Index: 1,
					},
				},
			},
		},
		intrans: map[int]int{
			0: 0,
			1: 1,
		},
	},
	{
		name: "sort outputs on value",
		tx: &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					Value:    2,
					PkScript: []byte{0x0},
				},
				{
					Value:    1,
					PkScript: []byte{0x0},
				},
			},
		},
		cltvs: []uint32{0, 0},
		outtrans: map[int]int{
			0: 1,
			1: 0,
		},
	},
	{
		name: "sort outputs on pkscript",
		tx: &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					Value:    1,
					PkScript: []byte{0x2},
				},
				{
					Value:    1,
					PkScript: []byte{0x1},
				},
			},
		},
		cltvs: []uint32{0, 0},
		outtrans: map[int]int{
			0: 1,
			1: 0,
		},
	},
	{
		name: "sort outputs on cltv",
		tx: &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					Value:    1,
					PkScript: []byte{0x1},
				},
				{
					Value:    1,
					PkScript: []byte{0x1},
				},
			},
		},
		cltvs: []uint32{2, 1},
		outtrans: map[int]int{
			0: 1,
			1: 0,
		},
	},
	{
		name: "sort complex outputs",
		tx: &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					Value:    100000,
					PkScript: []byte{0x01, 0x02},
				},
				{
					Value:    200000,
					PkScript: []byte{0x03, 0x02},
				},
				{
					Value:    1000,
					PkScript: []byte{0x03},
				},
				{
					Value:    1000,
					PkScript: []byte{0x02},
				},
				{
					Value:    1000,
					PkScript: []byte{0x1},
				},
				{
					Value:    1000,
					PkScript: []byte{0x1},
				},
			},
		},
		cltvs: []uint32{0, 0, 100, 90, 70, 80},
		outtrans: map[int]int{
			0: 4,
			1: 5,
			2: 3,
			3: 2,
			4: 0,
			5: 1,
		},
	},
	{
		name: "outputs already sorted",
		tx: &wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					Value:    1,
					PkScript: []byte{0x1},
				},
				{
					Value:    1,
					PkScript: []byte{0x1},
				},
			},
		},
		cltvs: []uint32{1, 2},
		outtrans: map[int]int{
			0: 0,
			1: 1,
		},
	},
}

// TestCommitSort asserts that the outputs of a transaction are properly sorted
// using InPlaceCommitSort. The outputs should always be sorted by value, then
// lexicographically by pkscript, and then CLTV value.
func TestCommitSort(t *testing.T) {
	for _, test := range commitSortTests {
		t.Run(test.name, func(t *testing.T) {
			expTxIns := test.expTxIns()
			expTxOuts := test.expTxOuts()

			lnwallet.InPlaceCommitSort(test.tx, test.cltvs)

			if !reflect.DeepEqual(test.tx.TxIn, expTxIns) {
				t.Fatalf("commit inputs mismatch, want: %v, "+
					"got: %v", expTxIns, test.tx.TxIn)
			}

			if !reflect.DeepEqual(test.tx.TxOut, expTxOuts) {
				t.Fatalf("commit outputs mismatch, want: %v, "+
					"got: %v", expTxOuts, test.tx.TxOut)
			}
		})
	}
}
