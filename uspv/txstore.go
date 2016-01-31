package uspv

import (
	"bytes"
	"fmt"
	"log"

	"github.com/btcsuite/btcd/txscript"

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

// Ingest a tx into wallet, dealing with both gains and losses
func (t *TxStore) AckTxz(tx *wire.MsgTx) (uint32, error) {
	var ioHits uint32 // number of utxos changed due to this tx

	inTxid := tx.TxSha()
	height, ok := t.OKTxids[inTxid]
	if !ok {
		log.Printf("%s", TxToString(tx))
		return 0, fmt.Errorf("tx %s not in OKTxids.", inTxid.String())
	}
	delete(t.OKTxids, inTxid) // don't need anymore
	hitsGained, err := t.AbsorbTx(tx, height)
	if err != nil {
		return 0, err
	}
	hitsLost, err := t.ExpellTx(tx, height)
	if err != nil {
		return 0, err
	}
	ioHits = hitsGained + hitsLost

	//	fmt.Printf("ingested tx %s total amt %d\n", inTxid.String(), t.Sum)
	return ioHits, nil
}

// AbsorbTx Absorbs money into wallet from a tx. returns number of
// new utxos absorbed.
func (t *TxStore) AbsorbTx(tx *wire.MsgTx, height int32) (uint32, error) {
	if tx == nil {
		return 0, fmt.Errorf("Tried to add nil tx")
	}
	newTxid := tx.TxSha()
	var hits uint32 // how many outputs of this tx are ours
	var acq int64   // total acquirement from this tx
	// check if any of the tx's outputs match my known outpoints
	for i, out := range tx.TxOut { // in each output of tx
		dup := false // start by assuming its new until found duplicate
		newOp := wire.NewOutPoint(&newTxid, uint32(i))
		// first look for dupes -- already known outpoints.
		// if we find a dupe here overwrite it to the DB.
		for _, u := range t.Utxos {
			dup = OutPointsEqual(*newOp, u.Op) // is this outpoint known?
			if dup {                           // found dupe
				fmt.Printf(" %s is dupe\t", newOp.String())
				hits++              // thought a dupe, still a hit
				u.AtHeight = height // ONLY difference is height
				// save modified utxo to db, overwriting old one
				err := t.SaveUtxo(u)
				if err != nil {
					return 0, err
				}
				break // out of the t.Utxo range loop
			}
		}
		if dup {
			// if we found the outpoint to be a dup above, don't add it again
			// when it matches an address, just go to the next outpoint
			continue
		}
		// check if this is a new txout matching one of my addresses
		for _, a := range t.Adrs { // compare to each adr we have
			// check for full script to eliminate false positives
			aPKscript, err := txscript.PayToAddrScript(a.PkhAdr)
			if err != nil {
				return 0, err
			}
			if bytes.Equal(out.PkScript, aPKscript) { // hit
				// already checked for dupes, so this must be a new outpoint
				var newu Utxo
				newu.AtHeight = height
				newu.KeyIdx = a.KeyIdx
				newu.Value = out.Value

				var newop wire.OutPoint
				newop.Hash = tx.TxSha()
				newop.Index = uint32(i)
				newu.Op = newop
				err = t.SaveUtxo(&newu)
				if err != nil {
					return 0, err
				}

				acq += out.Value
				hits++
				t.Utxos = append(t.Utxos, &newu) // always add new utxo
				break
			}
		}
	}
	//	log.Printf("%d hits, acquired %d", hits, acq)
	t.Sum += acq
	return hits, nil
}

// Expell money from wallet due to a tx
func (t *TxStore) ExpellTx(tx *wire.MsgTx, height int32) (uint32, error) {
	if tx == nil {
		return 0, fmt.Errorf("Tried to add nil tx")
	}
	var hits uint32
	var loss int64

	for _, in := range tx.TxIn {
		for i, myutxo := range t.Utxos {
			if OutPointsEqual(myutxo.Op, in.PreviousOutPoint) {
				hits++
				loss += myutxo.Value
				err := t.MarkSpent(*myutxo, height, tx)
				if err != nil {
					return 0, err
				}
				// delete from my in-ram utxo set
				t.Utxos = append(t.Utxos[:i], t.Utxos[i+1:]...)
			}
		}
	}
	//	log.Printf("%d hits, lost %d", hits, loss)
	t.Sum -= loss
	return hits, nil
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
