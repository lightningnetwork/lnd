package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"sync"

	"github.com/roasbeef/btcd/chaincfg"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/bloom"
	"github.com/roasbeef/btcutil/hdkeychain"
)

type TxStore struct {
	OKTxids map[wire.ShaHash]int32 // known good txids and their heights
	OKMutex sync.Mutex

	Adrs    []MyAdr  // endeavouring to acquire capital
	StateDB *bolt.DB // place to write all this down

	localFilter *bloom.Filter // local bloom filter for hard mode

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

	//		IsCoinbase bool          // can't spend for a while
	IsWit bool // true if p2wpkh output
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
		return nil, fmt.Errorf("no address to filter for")
	}

	// get all utxos to add outpoints to filter
	allUtxos, err := t.GetAllUtxos()
	if err != nil {
		return nil, err
	}

	elem := uint32(len(t.Adrs) + len(allUtxos))
	f := bloom.NewFilter(elem, 0, 0.000001, wire.BloomUpdateAll)

	// note there could be false positives since we're just looking
	// for the 20 byte PKH without the opcodes.
	for _, a := range t.Adrs { // add 20-byte pubkeyhash
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
	str := fmt.Sprintf("size %d vsize %d wsize %d locktime %d flag %x txid %s\n",
		tx.SerializeSize(), tx.VirtualSize(), tx.SerializeSizeWitness(),
		tx.LockTime, tx.Flags, tx.TxSha().String())
	for i, in := range tx.TxIn {
		str += fmt.Sprintf("Input %d spends %s\n", i, in.PreviousOutPoint.String())
		str += fmt.Sprintf("\tSigScript: %x\n", in.SignatureScript)
		for j, wit := range in.Witness {
			str += fmt.Sprintf("\twitness %d: %x\n", j, wit)
		}
	}
	for i, out := range tx.TxOut {
		if out != nil {
			str += fmt.Sprintf("output %d script: %x amt: %d\n",
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

/*----- serialization for utxos ------- */
/* Utxos serialization:
byte length   desc   at offset

32	txid		0
4	idx		32
4	height	36
4	keyidx	40
8	amt		44
1	flag		52

end len 	53
*/

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
	// last byte indicates tx witness flags ( tx[5] from serialized tx)
	// write a 1 at the end for p2wpkh (same as flags byte)
	witByte := byte(0x00)
	if u.IsWit {
		witByte = 0x01
	}
	err = buf.WriteByte(witByte)
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
	if buf.Len() < 53 { // utxos are 53 bytes
		return u, fmt.Errorf("Got %d bytes for utxo, expect 53", buf.Len())
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
	// read 1 byte witness flags
	witByte, err := buf.ReadByte()
	if err != nil {
		return u, err
	}
	if witByte != 0x00 {
		u.IsWit = true
	}

	return u, nil
}

/*----- serialization for stxos ------- */
/* Stxo serialization:
byte length   desc   at offset

53	utxo		0
4	sheight	53
32	stxid	57

end len 	89
*/

// ToBytes turns an Stxo into some bytes.
// prevUtxo serialization, then spendheight [4], spendtxid [32]
func (s *Stxo) ToBytes() ([]byte, error) {
	var buf bytes.Buffer
	// first serialize the utxo part
	uBytes, err := s.Utxo.ToBytes()
	if err != nil {
		return nil, err
	}
	// write that into the buffer first
	_, err = buf.Write(uBytes)
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
// first take the first 53 bytes as a utxo, then the next 36 for how it's spent.
func StxoFromBytes(b []byte) (Stxo, error) {
	var s Stxo
	if len(b) < 89 {
		return s, fmt.Errorf("Got %d bytes for stxo, expect 89", len(b))
	}

	u, err := UtxoFromBytes(b[:53])
	if err != nil {
		return s, err
	}
	s.Utxo = u // assign the utxo

	buf := bytes.NewBuffer(b[53:]) // make buffer for spend data

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
