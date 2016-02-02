package uspv

import (
	"bytes"
	"fmt"
	"log"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
)

type TxStore struct {
	KnownTxids []*wire.ShaHash
	Utxos      []Utxo  // stacks on stacks
	Sum        int64   // racks on racks
	Adrs       []MyAdr // endeavouring to acquire capital
}

type Utxo struct { // cash money.
	// combo of outpoint and txout which has all the info needed to spend
	Op       wire.OutPoint
	Txo      wire.TxOut
	AtHeight uint32 // block height where this tx was confirmed, 0 for unconf
	KeyIdx   uint32 // index for private key needed to sign / spend
}

type MyAdr struct { // an address I have the private key for
	btcutil.Address
	KeyIdx uint32 // index for private key needed to sign / spend
}

// add addresses into the TxStore
func (t *TxStore) AddAdr(a btcutil.Address, kidx uint32) {
	var ma MyAdr
	ma.Address = a
	ma.KeyIdx = kidx
	t.Adrs = append(t.Adrs, ma)
	return
}

// add txid of interest
func (t *TxStore) AddTxid(txid *wire.ShaHash) error {
	if txid == nil {
		return fmt.Errorf("tried to add nil txid")
	}
	t.KnownTxids = append(t.KnownTxids, txid)
	return nil
}

// ... or I'm gonna fade away
func (t *TxStore) GimmeFilter() (*bloom.Filter, error) {
	if len(t.Adrs) == 0 {
		return nil, fmt.Errorf("no addresses to filter for")
	}
	f := bloom.NewFilter(uint32(len(t.Adrs)), 0, 0.001, wire.BloomUpdateNone)
	for _, a := range t.Adrs {
		f.Add(a.ScriptAddress())
	}
	return f, nil
}

// Ingest a tx into wallet, dealing with both gains and losses
func (t *TxStore) IngestTx(tx *wire.MsgTx) error {
	var match bool
	inTxid := tx.TxSha()
	for _, ktxid := range t.KnownTxids {
		if inTxid.IsEqual(ktxid) {
			match = true
			break // found tx match,
		}
	}
	if !match {
		return fmt.Errorf("we don't care about tx %s", inTxid.String())
	}

	err := t.AbsorbTx(tx)
	if err != nil {
		return err
	}
	err = t.ExpellTx(tx)
	if err != nil {
		return err
	}
	//	fmt.Printf("ingested tx %s total amt %d\n", inTxid.String(), t.Sum)
	return nil
}

// Absorb money into wallet from a tx
func (t *TxStore) AbsorbTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("Tried to add nil tx")
	}
	var hits uint32
	var acq int64
	// check if any of the tx's outputs match my adrs
	for i, out := range tx.TxOut { // in each output of tx
		for _, a := range t.Adrs { // compare to each adr we have
			// more correct would be to check for full script
			// contains could have false positive? (p2sh/p2pkh same hash ..?)
			if bytes.Contains(out.PkScript, a.ScriptAddress()) { // hit
				hits++
				acq += out.Value
				var newu Utxo
				newu.KeyIdx = a.KeyIdx
				newu.Txo = *out

				var newop wire.OutPoint
				newop.Hash = tx.TxSha()
				newop.Index = uint32(i)
				newu.Op = newop

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
func (t *TxStore) ExpellTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("Tried to add nil tx")
	}
	var hits uint32
	var loss int64

	for _, in := range tx.TxIn {
		for i, myutxo := range t.Utxos {
			if myutxo.Op == in.PreviousOutPoint {

				hits++
				loss += myutxo.Txo.Value
				// delete from my utxo set
				t.Utxos = append(t.Utxos[:i], t.Utxos[i+1:]...)
			}
		}
	}
	log.Printf("%d hits, lost %d", hits, loss)
	t.Sum -= loss
	return nil
}
