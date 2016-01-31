package uspv

import (
	"fmt"
	"log"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
	"github.com/btcsuite/btcutil/hdkeychain"
)

type TxStore struct {
	OKTxids map[wire.ShaHash]int32 // known good txids and their heights

	Utxos   []*Utxo  // stacks on stacks
	Sum     int64    // racks on racks
	Adrs    []MyAdr  // endeavouring to acquire capital
	LastIdx uint32   // should equal len(Adrs)
	StateDB *bolt.DB // place to write all this down
	// this is redundant with the SPVCon param... ugly and should be taken out
	Param *chaincfg.Params // network parameters (testnet3, testnetL)

	// From here, comes everything. It's a secret to everybody.
	rootPrivKey *hdkeychain.ExtendedKey
}

type Utxo struct { // cash money.
	Op wire.OutPoint // where

	// all the info needed to spend
	AtHeight int32  // block height where this tx was confirmed, 0 for unconf
	KeyIdx   uint32 // index for private key needed to sign / spend
	Value    int64  // higher is better

	//	IsCoinbase bool          // can't spend for a while
}

// Stxo is a utxo that has moved on.
type Stxo struct {
	Utxo                     // when it used to be a utxo
	SpendHeight int32        // height at which it met its demise
	SpendTxid   wire.ShaHash // the tx that consumed it
}

type MyAdr struct { // an address I have the private key for
	PkhAdr btcutil.Address
	KeyIdx uint32 // index for private key needed to sign / spend
	// ^^ this is kindof redundant because it'll just be their position
	// inside the Adrs slice, right? leave for now
}

func NewTxStore(rootkey *hdkeychain.ExtendedKey) TxStore {
	var txs TxStore
	txs.rootPrivKey = rootkey
	txs.OKTxids = make(map[wire.ShaHash]int32)
	return txs
}

// add addresses into the TxStore in memory
func (t *TxStore) AddAdr(a btcutil.Address, kidx uint32) {
	var ma MyAdr
	ma.PkhAdr = a
	ma.KeyIdx = kidx
	t.Adrs = append(t.Adrs, ma)
	return
}

// add txid of interest
func (t *TxStore) AddTxid(txid *wire.ShaHash, height int32) error {
	if txid == nil {
		return fmt.Errorf("tried to add nil txid")
	}
	log.Printf("added %s to OKTxids at height %d\n", txid.String(), height)
	t.OKTxids[*txid] = height
	return nil
}

// ... or I'm gonna fade away
func (t *TxStore) GimmeFilter() (*bloom.Filter, error) {
	if len(t.Adrs) == 0 {
		return nil, fmt.Errorf("no addresses to filter for")
	}
	// add addresses to look for incoming
	nutxo, err := t.NumUtxos()
	if err != nil {
		return nil, err
	}

	elem := uint32(len(t.Adrs)) + nutxo
	f := bloom.NewFilter(elem, 0, 0.000001, wire.BloomUpdateAll)
	for _, a := range t.Adrs {
		f.Add(a.PkhAdr.ScriptAddress())
	}

	// get all utxos to add outpoints to filter
	allUtxos, err := t.GetAllUtxos()
	if err != nil {
		return nil, err
	}

	for _, u := range allUtxos {
		f.AddOutPoint(&u.Op)
	}

	return f, nil
}

// need this because before I was comparing pointers maybe?
// so they were the same outpoint but stored in 2 places so false negative?
func OutPointsEqual(a, b wire.OutPoint) bool {
	if !a.Hash.IsEqual(&b.Hash) {
		return false
	}
	return a.Index == b.Index
}

// TxToString prints out some info about a transaction. for testing / debugging
func TxToString(tx *wire.MsgTx) string {
	str := "\t\t\t - Tx - \n"
	for i, in := range tx.TxIn {
		str += fmt.Sprintf("Input %d: %s\n", i, in.PreviousOutPoint.String())
		str += fmt.Sprintf("SigScript for input %d: %x\n", i, in.SignatureScript)
	}
	for i, out := range tx.TxOut {
		if out != nil {
			str += fmt.Sprintf("\toutput %d script: %x amt: %d\n",
				i, out.PkScript, out.Value)
		} else {
			str += fmt.Sprintf("output %d nil (WARNING)\n", i)
		}
	}
	return str
}
