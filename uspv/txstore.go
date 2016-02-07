package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
	"github.com/btcsuite/btcutil/hdkeychain"
)

type TxStore struct {
	OKTxids map[wire.ShaHash]int32 // known good txids and their heights
	OKMutex sync.Mutex

	Adrs    []MyAdr  // endeavouring to acquire capital
	StateDB *bolt.DB // place to write all this down

	// Params live here, not SCon
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

func NewTxStore(rootkey *hdkeychain.ExtendedKey, p *chaincfg.Params) TxStore {
	var txs TxStore
	txs.rootPrivKey = rootkey
	txs.Param = p
	txs.OKTxids = make(map[wire.ShaHash]int32)
	return txs
}

// add txid of interest
func (t *TxStore) AddTxid(txid *wire.ShaHash, height int32) error {
	if txid == nil {
		return fmt.Errorf("tried to add nil txid")
	}
	log.Printf("added %s to OKTxids at height %d\n", txid.String(), height)
	t.OKMutex.Lock()
	t.OKTxids[*txid] = height
	t.OKMutex.Unlock()
	return nil
}

// ... or I'm gonna fade away
func (t *TxStore) GimmeFilter() (*bloom.Filter, error) {
	if len(t.Adrs) == 0 {
		return nil, fmt.Errorf("no addresses to filter for")
	}

	// get all utxos to add outpoints to filter
	allUtxos, err := t.GetAllUtxos()
	if err != nil {
		return nil, err
	}

	elem := uint32(len(t.Adrs) + len(allUtxos))
	f := bloom.NewFilter(elem, 0, 0.000001, wire.BloomUpdateAll)
	for _, a := range t.Adrs {
		f.Add(a.PkhAdr.ScriptAddress())
	}

	for _, u := range allUtxos {
		f.AddOutPoint(&u.Op)
	}

	return f, nil
}

// GetDoubleSpends takes a transaction and compares it with
// all transactions in the db.  It returns a slice of all txids in the db
// which are double spent by the received tx.
func CheckDoubleSpends(
	argTx *wire.MsgTx, txs []*wire.MsgTx) ([]*wire.ShaHash, error) {

	var dubs []*wire.ShaHash // slice of all double-spent txs
	argTxid := argTx.TxSha()

	for _, compTx := range txs {
		compTxid := compTx.TxSha()
		// check if entire tx is dup
		if argTxid.IsEqual(&compTxid) {
			return nil, fmt.Errorf("tx %s is dup", argTxid.String())
		}
		// not dup, iterate through inputs of argTx
		for _, argIn := range argTx.TxIn {
			// iterate through inputs of compTx
			for _, compIn := range compTx.TxIn {
				if OutPointsEqual(
					argIn.PreviousOutPoint, compIn.PreviousOutPoint) {
					// found double spend
					dubs = append(dubs, &compTxid)
					break // back to argIn loop
				}
			}
		}
	}
	return dubs, nil
}

// TxToString prints out some info about a transaction. for testing / debugging
func TxToString(tx *wire.MsgTx) string {
	str := fmt.Sprintf("\t - Tx %s\n", tx.TxSha().String())
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

// need this because before I was comparing pointers maybe?
// so they were the same outpoint but stored in 2 places so false negative?
func OutPointsEqual(a, b wire.OutPoint) bool {
	if !a.Hash.IsEqual(&b.Hash) {
		return false
	}
	return a.Index == b.Index
}

/*----- serialization for tx outputs ------- */

// outPointToBytes turns an outpoint into 36 bytes.
func outPointToBytes(op *wire.OutPoint) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.Write(op.Hash.Bytes())
	if err != nil {
		return nil, err
	}
	// write 4 byte outpoint index within the tx to spend
	err = binary.Write(&buf, binary.BigEndian, op.Index)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ToBytes turns a Utxo into some bytes.
// note that the txid is the first 36 bytes and in our use cases will be stripped
// off, but is left here for other applications
func (u *Utxo) ToBytes() ([]byte, error) {
	var buf bytes.Buffer
	// write 32 byte txid of the utxo
	_, err := buf.Write(u.Op.Hash.Bytes())
	if err != nil {
		return nil, err
	}
	// write 4 byte outpoint index within the tx to spend
	err = binary.Write(&buf, binary.BigEndian, u.Op.Index)
	if err != nil {
		return nil, err
	}
	// write 4 byte height of utxo
	err = binary.Write(&buf, binary.BigEndian, u.AtHeight)
	if err != nil {
		return nil, err
	}
	// write 4 byte key index of utxo
	err = binary.Write(&buf, binary.BigEndian, u.KeyIdx)
	if err != nil {
		return nil, err
	}
	// write 8 byte amount of money at the utxo
	err = binary.Write(&buf, binary.BigEndian, u.Value)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// UtxoFromBytes turns bytes into a Utxo.  Note it wants the txid and outindex
// in the first 36 bytes, which isn't stored that way in the boldDB,
// but can be easily appended.
func UtxoFromBytes(b []byte) (Utxo, error) {
	var u Utxo
	if b == nil {
		return u, fmt.Errorf("nil input slice")
	}
	buf := bytes.NewBuffer(b)
	if buf.Len() < 52 { // utxos are 52 bytes
		return u, fmt.Errorf("Got %d bytes for utxo, expect 52", buf.Len())
	}
	// read 32 byte txid
	err := u.Op.Hash.SetBytes(buf.Next(32))
	if err != nil {
		return u, err
	}
	// read 4 byte outpoint index within the tx to spend
	err = binary.Read(buf, binary.BigEndian, &u.Op.Index)
	if err != nil {
		return u, err
	}
	// read 4 byte height of utxo
	err = binary.Read(buf, binary.BigEndian, &u.AtHeight)
	if err != nil {
		return u, err
	}
	// read 4 byte key index of utxo
	err = binary.Read(buf, binary.BigEndian, &u.KeyIdx)
	if err != nil {
		return u, err
	}
	// read 8 byte amount of money at the utxo
	err = binary.Read(buf, binary.BigEndian, &u.Value)
	if err != nil {
		return u, err
	}
	return u, nil
}

// ToBytes turns an Stxo into some bytes.
// outpoint txid, outpoint idx, height, key idx, amt, spendheight, spendtxid
func (s *Stxo) ToBytes() ([]byte, error) {
	var buf bytes.Buffer
	// write 32 byte txid of the utxo
	_, err := buf.Write(s.Op.Hash.Bytes())
	if err != nil {
		return nil, err
	}
	// write 4 byte outpoint index within the tx to spend
	err = binary.Write(&buf, binary.BigEndian, s.Op.Index)
	if err != nil {
		return nil, err
	}
	// write 4 byte height of utxo
	err = binary.Write(&buf, binary.BigEndian, s.AtHeight)
	if err != nil {
		return nil, err
	}
	// write 4 byte key index of utxo
	err = binary.Write(&buf, binary.BigEndian, s.KeyIdx)
	if err != nil {
		return nil, err
	}
	// write 8 byte amount of money at the utxo
	err = binary.Write(&buf, binary.BigEndian, s.Value)
	if err != nil {
		return nil, err
	}
	// write 4 byte height where the txo was spent
	err = binary.Write(&buf, binary.BigEndian, s.SpendHeight)
	if err != nil {
		return nil, err
	}
	// write 32 byte txid of the spending transaction
	_, err = buf.Write(s.SpendTxid.Bytes())
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// StxoFromBytes turns bytes into a Stxo.
func StxoFromBytes(b []byte) (Stxo, error) {
	var s Stxo
	if b == nil {
		return s, fmt.Errorf("nil input slice")
	}
	buf := bytes.NewBuffer(b)
	if buf.Len() < 88 { // stxos are 88 bytes
		return s, fmt.Errorf("Got %d bytes for stxo, expect 88", buf.Len())
	}
	// read 32 byte txid
	err := s.Op.Hash.SetBytes(buf.Next(32))
	if err != nil {
		return s, err
	}
	// read 4 byte outpoint index within the tx to spend
	err = binary.Read(buf, binary.BigEndian, &s.Op.Index)
	if err != nil {
		return s, err
	}
	// read 4 byte height of utxo
	err = binary.Read(buf, binary.BigEndian, &s.AtHeight)
	if err != nil {
		return s, err
	}
	// read 4 byte key index of utxo
	err = binary.Read(buf, binary.BigEndian, &s.KeyIdx)
	if err != nil {
		return s, err
	}
	// read 8 byte amount of money at the utxo
	err = binary.Read(buf, binary.BigEndian, &s.Value)
	if err != nil {
		return s, err
	}
	// read 4 byte spend height
	err = binary.Read(buf, binary.BigEndian, &s.SpendHeight)
	if err != nil {
		return s, err
	}
	// read 32 byte txid
	err = s.SpendTxid.SetBytes(buf.Next(32))
	if err != nil {
		return s, err
	}
	return s, nil
}
