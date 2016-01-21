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

	Utxos   []Utxo   // stacks on stacks
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
	// all the info needed to spend
	AtHeight int32  // block height where this tx was confirmed, 0 for unconf
	KeyIdx   uint32 // index for private key needed to sign / spend
	Value    int64  // higher is better

	Op wire.OutPoint // where
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
	log.Printf("added %s at height %d\n", txid.String(), height)
	t.OKTxids[*txid] = height
	return nil
}

// ... or I'm gonna fade away
func (t *TxStore) GimmeFilter() (*bloom.Filter, error) {
	if len(t.Adrs) == 0 {
		return nil, fmt.Errorf("no addresses to filter for")
	}
	f := bloom.NewFilter(uint32(len(t.Adrs)), 0, 0.001, wire.BloomUpdateNone)
	for _, a := range t.Adrs {
		f.Add(a.PkhAdr.ScriptAddress())
	}
	return f, nil
}

// Ingest a tx into wallet, dealing with both gains and losses
func (t *TxStore) IngestTx(tx *wire.MsgTx) error {
	inTxid := tx.TxSha()
	height, ok := t.OKTxids[inTxid]
	if !ok {
		log.Printf("False postive tx? %s", TxToString(tx))
		return fmt.Errorf("we don't care about tx %s", inTxid.String())
	}

	err := t.AbsorbTx(tx, height)
	if err != nil {
		return err
	}
	err = t.ExpellTx(tx, height)
	if err != nil {
		return err
	}
	//	fmt.Printf("ingested tx %s total amt %d\n", inTxid.String(), t.Sum)
	return nil
}

// Absorb money into wallet from a tx
func (t *TxStore) AbsorbTx(tx *wire.MsgTx, height int32) error {
	if tx == nil {
		return fmt.Errorf("Tried to add nil tx")
	}
	var hits uint32
	var acq int64
	// check if any of the tx's outputs match my adrs
	for i, out := range tx.TxOut { // in each output of tx
		for _, a := range t.Adrs { // compare to each adr we have
			// check for full script to eliminate false positives
			aPKscript, err := txscript.PayToAddrScript(a.PkhAdr)
			if err != nil {
				return err
			}
			if bytes.Equal(out.PkScript, aPKscript) { // hit
				hits++
				acq += out.Value
				var newu Utxo
				newu.AtHeight = height
				newu.KeyIdx = a.KeyIdx
				newu.Value = out.Value

				var newop wire.OutPoint
				newop.Hash = tx.TxSha()
				newop.Index = uint32(i)
				newu.Op = newop
				err := newu.SaveToDB(t.StateDB)
				if err != nil {
					return err
				}
				t.Sum += newu.Value
				t.Utxos = append(t.Utxos, newu)
				break
			}
		}
	}
	log.Printf("%d hits, acquired %d", hits, acq)
	t.Sum += acq
	return nil
}

// Expell money from wallet due to a tx
func (t *TxStore) ExpellTx(tx *wire.MsgTx, height int32) error {
	if tx == nil {
		return fmt.Errorf("Tried to add nil tx")
	}
	var hits uint32
	var loss int64

	for _, in := range tx.TxIn {
		for i, myutxo := range t.Utxos {
			if myutxo.Op == in.PreviousOutPoint {
				hits++
				loss += myutxo.Value
				err := t.MarkSpent(&myutxo.Op, height, tx)
				if err != nil {
					return err
				}
				// delete from my in-ram utxo set
				t.Utxos = append(t.Utxos[:i], t.Utxos[i+1:]...)
			}
		}
	}
	log.Printf("%d hits, lost %d", hits, loss)
	t.Sum -= loss
	return nil
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
