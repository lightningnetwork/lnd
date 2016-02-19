package uspv

import (
	"bytes"
	"fmt"
	"log"
	"sort"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/txsort"
)

func (s *SPVCon) PongBack(nonce uint64) {
	mpong := wire.NewMsgPong(nonce)

	s.outMsgQueue <- mpong
	return
}

func (s *SPVCon) SendFilter(f *bloom.Filter) {
	s.outMsgQueue <- f.MsgFilterLoad()

	return
}

// Rebroadcast sends an inv message of all the unconfirmed txs the db is
// aware of.  This is called after every sync.  Only txids so hopefully not
// too annoying for nodes.
func (s *SPVCon) Rebroadcast() {
	// get all unconfirmed txs
	invMsg, err := s.TS.GetPendingInv()
	if err != nil {
		log.Printf("Rebroadcast error: %s", err.Error())
	}
	if len(invMsg.InvList) == 0 { // nothing to broadcast, so don't
		return
	}
	s.outMsgQueue <- invMsg
	return
}

// make utxo slices sortable
type utxoSlice []Utxo

// Sort utxos just like txins -- Len, Less, Swap
func (s utxoSlice) Len() int      { return len(s) }
func (s utxoSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// outpoint sort; First input hash (reversed / rpc-style), then index.
func (s utxoSlice) Less(i, j int) bool {
	// Input hashes are the same, so compare the index.
	ihash := s[i].Op.Hash
	jhash := s[j].Op.Hash
	if ihash == jhash {
		return s[i].Op.Index < s[j].Op.Index
	}
	// At this point, the hashes are not equal, so reverse them to
	// big-endian and return the result of the comparison.
	const hashSize = wire.HashSize
	for b := 0; b < hashSize/2; b++ {
		ihash[b], ihash[hashSize-1-b] = ihash[hashSize-1-b], ihash[b]
		jhash[b], jhash[hashSize-1-b] = jhash[hashSize-1-b], jhash[b]
	}
	return bytes.Compare(ihash[:], jhash[:]) == -1
}

func (s *SPVCon) NewOutgoingTx(tx *wire.MsgTx) error {
	txid := tx.TxSha()
	// assign height of zero for txs we create
	err := s.TS.AddTxid(&txid, 0)
	if err != nil {
		return err
	}
	_, err = s.TS.Ingest(tx, 0) // our own tx; don't keep track of false positives
	if err != nil {
		return err
	}
	// make an inv message instead of a tx message to be polite
	iv1 := wire.NewInvVect(wire.InvTypeWitnessTx, &txid)
	invMsg := wire.NewMsgInv()
	err = invMsg.AddInvVect(iv1)
	if err != nil {
		return err
	}
	s.outMsgQueue <- invMsg
	return nil
}

// SendCoins does send coins, but it's very rudimentary
// wit makes it into p2wpkh.  Which is not yet spendable.
func (s *SPVCon) SendCoins(adr btcutil.Address, sendAmt int64) error {

	var err error
	var score int64
	allUtxos, err := s.TS.GetAllUtxos()
	if err != nil {
		return err
	}

	for _, utxo := range allUtxos {
		score += utxo.Value
	}
	// important rule in bitcoin, output total > input total is invalid.
	if sendAmt > score {
		return fmt.Errorf("trying to send %d but %d available.",
			sendAmt, score)
	}

	///////////////////
	tx := wire.NewMsgTx() // make new tx
	// make address script 76a914...88ac or 0014...
	outAdrScript, err := txscript.PayToAddrScript(adr)
	if err != nil {
		return err
	}

	////////////////////////////

	// generate a utxo slice for your inputs
	var ins utxoSlice

	// add utxos until we've had enough
	nokori := sendAmt // nokori is how much is needed on input side
	for _, utxo := range allUtxos {
		// yeah, lets add this utxo!
		ins = append(ins, *utxo)
		nokori -= utxo.Value
		if nokori < -10000 { // minimum overage / fee is 10K now
			break
		}
	}

	// sort utxos on the input side
	sort.Sort(ins)

	// make user specified txout and add to tx
	txout := wire.NewTxOut(sendAmt, outAdrScript)
	tx.AddTxOut(txout)
	// see if there's enough left to also add a change output
	if nokori < -200000 {
		changeOld, err := s.TS.NewAdr() // change is witnessy
		if err != nil {
			return err
		}
		changeAdr, err := btcutil.NewAddressWitnessPubKeyHash(
			changeOld.ScriptAddress(), s.TS.Param)
		if err != nil {
			return err
		}

		changeScript, err := txscript.PayToAddrScript(changeAdr)
		if err != nil {
			return err
		}
		changeOut := wire.NewTxOut((-100000)-nokori, changeScript)
		tx.AddTxOut(changeOut)
	}

	// generate previous pkscripts for all the (now sorted) utxos
	// then make txins with the utxo and prevpk, and insert them into the tx
	for _, in := range ins {
		var prevPKs []byte

		// if wit utxo, convert address to generate pkscript
		if in.IsWit {
			wa, err := btcutil.NewAddressWitnessPubKeyHash(
				s.TS.Adrs[in.KeyIdx].PkhAdr.ScriptAddress(), s.TS.Param)
			prevPKs, err = txscript.PayToAddrScript(wa)
			if err != nil {
				return err
			}
		} else { // otherwise generate directly
			prevPKs, err = txscript.PayToAddrScript(
				s.TS.Adrs[in.KeyIdx].PkhAdr)
			if err != nil {
				return err
			}
		}
		tx.AddTxIn(wire.NewTxIn(&in.Op, prevPKs, nil))
	}
	// sort tx -- this only will change txouts since inputs are already sorted
	txsort.InPlaceSort(tx)

	// tx is ready for signing,
	sigStash := make([][]byte, len(ins))
	for i, txin := range tx.TxIn {
		// pick key
		child, err := s.TS.rootPrivKey.Child(
			ins[i].KeyIdx + hdkeychain.HardenedKeyStart)
		if err != nil {
			return err
		}
		priv, err := child.ECPrivKey()
		if err != nil {
			return err
		}

		// This is where witness based sighash types need to happen
		// sign into stash
		sigStash[i], err = txscript.SignatureScript(
			tx, i, txin.SignatureScript, txscript.SigHashAll, priv, true)
		if err != nil {
			return err
		}
	}
	// swap sigs into sigScripts in txins
	for i, txin := range tx.TxIn {
		txin.SignatureScript = sigStash[i]
	}

	fmt.Printf("tx: %s", TxToString(tx))
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
	tx.Serialize(buf)
	fmt.Printf("tx: %x\n", buf.Bytes())

	// send it out on the wire.  hope it gets there.
	// we should deal with rejects.  Don't yet.
	err = s.NewOutgoingTx(tx)
	if err != nil {
		return err
	}
	return nil
}

// SignThis isn't used anymore...
func (t *TxStore) SignThis(tx *wire.MsgTx) error {
	fmt.Printf("-= SignThis =-\n")

	// sort tx before signing.
	txsort.InPlaceSort(tx)

	sigs := make([][]byte, len(tx.TxIn))
	// first iterate over each input
	for j, in := range tx.TxIn {
		for k := uint32(0); k < uint32(len(t.Adrs)); k++ {
			child, err := t.rootPrivKey.Child(k + hdkeychain.HardenedKeyStart)
			if err != nil {
				return err
			}
			myadr, err := child.Address(t.Param)
			if err != nil {
				return err
			}
			adrScript, err := txscript.PayToAddrScript(myadr)
			if err != nil {
				return err
			}
			if bytes.Equal(adrScript, in.SignatureScript) {
				fmt.Printf("Hit; key %d matches input %d. Signing.\n", k, j)
				priv, err := child.ECPrivKey()
				if err != nil {
					return err
				}
				sigs[j], err = txscript.SignatureScript(
					tx, j, in.SignatureScript, txscript.SigHashAll, priv, true)
				if err != nil {
					return err
				}
				break
			}
		}
	}
	for i, s := range sigs {
		if s != nil {
			tx.TxIn[i].SignatureScript = s
		}
	}
	return nil
}
