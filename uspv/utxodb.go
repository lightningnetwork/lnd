package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/txscript"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"

	"github.com/boltdb/bolt"
)

var (
	BKTUtxos = []byte("DuffelBag") // leave the rest to collect interest
	BKTStxos = []byte("SpentTxs")  // for bookkeeping
	BKTTxns  = []byte("Txns")      // all txs we care about, for replays
	BKTState = []byte("MiscState") // last state of DB
	// these are in the state bucket
	KEYNumKeys   = []byte("NumKeys")   // number of keys used
	KEYTipHeight = []byte("TipHeight") // height synced to
)

func (ts *TxStore) OpenDB(filename string) error {
	var err error
	ts.StateDB, err = bolt.Open(filename, 0644, nil)
	if err != nil {
		return err
	}
	// create buckets if they're not already there
	return ts.StateDB.Update(func(btx *bolt.Tx) error {
		_, err = btx.CreateBucketIfNotExists(BKTUtxos)
		if err != nil {
			return err
		}
		_, err = btx.CreateBucketIfNotExists(BKTStxos)
		if err != nil {
			return err
		}
		_, err = btx.CreateBucketIfNotExists(BKTTxns)
		if err != nil {
			return err
		}
		_, err = btx.CreateBucketIfNotExists(BKTState)
		if err != nil {
			return err
		}
		return nil
	})
}

// NewAdr creates a new, never before seen address, and increments the
// DB counter as well as putting it in the ram Adrs store, and returns it
func (ts *TxStore) NewAdr() (*btcutil.AddressPubKeyHash, error) {
	if ts.Param == nil {
		return nil, fmt.Errorf("nil param")
	}
	n := uint32(len(ts.Adrs))
	priv, err := ts.rootPrivKey.Child(n + hdkeychain.HardenedKeyStart)
	if err != nil {
		return nil, err
	}

	newAdr, err := priv.Address(ts.Param)
	if err != nil {
		return nil, err
	}

	// total number of keys (now +1) into 4 bytes
	var buf bytes.Buffer
	err = binary.Write(&buf, binary.BigEndian, n+1)
	if err != nil {
		return nil, err
	}

	// write to db file
	err = ts.StateDB.Update(func(btx *bolt.Tx) error {
		sta := btx.Bucket(BKTState)
		return sta.Put(KEYNumKeys, buf.Bytes())
	})
	if err != nil {
		return nil, err
	}
	// add in to ram.
	ts.AddAdr(newAdr, n)
	return newAdr, nil
}

// SetBDay sets the birthday (birth height) of the db (really keyfile)
func (ts *TxStore) SetDBSyncHeight(n int32) error {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, n)

	return ts.StateDB.Update(func(btx *bolt.Tx) error {
		sta := btx.Bucket(BKTState)
		return sta.Put(KEYTipHeight, buf.Bytes())
	})
}

// SyncHeight returns the chain height to which the db has synced
func (ts *TxStore) GetDBSyncHeight() (int32, error) {
	var n int32
	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		sta := btx.Bucket(BKTState)
		if sta == nil {
			return fmt.Errorf("no state")
		}
		t := sta.Get(KEYTipHeight)

		if t == nil { // no height written, so 0
			return nil
		}

		// read 4 byte tip height to n
		err := binary.Read(bytes.NewBuffer(t), binary.BigEndian, &n)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// NumUtxos returns the number of utxos in the DB.
func (ts *TxStore) NumUtxos() (uint32, error) {
	var n uint32
	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		duf := btx.Bucket(BKTUtxos)
		if duf == nil {
			return fmt.Errorf("no duffel bag")
		}
		stats := duf.Stats()
		n = uint32(stats.KeyN)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (ts *TxStore) GetAllUtxos() ([]*Utxo, error) {
	var utxos []*Utxo
	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		duf := btx.Bucket(BKTUtxos)
		if duf == nil {
			return fmt.Errorf("no duffel bag")
		}
		return duf.ForEach(func(k, v []byte) error {
			// have to copy k and v here, otherwise append will crash it.
			// not quite sure why but append does weird stuff I guess.

			// create a new utxo
			x := make([]byte, len(k)+len(v))
			copy(x, k)
			copy(x[len(k):], v)
			newU, err := UtxoFromBytes(x)
			if err != nil {
				return err
			}
			// and add it to ram
			utxos = append(utxos, &newU)

			return nil
		})

		return nil
	})
	if err != nil {
		return nil, err
	}
	return utxos, nil
}

// PopulateAdrs just puts a bunch of adrs in ram; it doesn't touch the DB
func (ts *TxStore) PopulateAdrs(lastKey uint32) error {
	for k := uint32(0); k < lastKey; k++ {

		priv, err := ts.rootPrivKey.Child(k + hdkeychain.HardenedKeyStart)
		if err != nil {
			return err
		}

		newAdr, err := priv.Address(ts.Param)
		if err != nil {
			return err
		}

		ts.AddAdr(newAdr, k)
	}
	return nil
}

// Ingest puts a tx into the DB atomically.  This can result in a
// gain, a loss, or no result.  Gain or loss in satoshis is returned.
func (ts *TxStore) Ingest(tx *wire.MsgTx) (uint32, error) {
	var hits uint32
	var err error
	var spentOPs [][]byte
	var nUtxoBytes [][]byte

	// check that we have a height and tx has been SPV OK'd
	inTxid := tx.TxSha()
	height, ok := ts.OKTxids[inTxid]
	if !ok {
		return hits, fmt.Errorf("Ingest error: tx %s not in OKTxids.",
			inTxid.String())
	}

	// before entering into db, serialize all inputs of the ingested tx
	for _, txin := range tx.TxIn {
		nOP, err := outPointToBytes(&txin.PreviousOutPoint)
		if err != nil {
			return hits, err
		}
		spentOPs = append(spentOPs, nOP)
	}
	// also generate PKscripts for all addresses (maybe keep storing these?)
	for _, adr := range ts.Adrs {
		// iterate through all our addresses
		aPKscript, err := txscript.PayToAddrScript(adr.PkhAdr)
		if err != nil {
			return hits, err
		}
		// iterate through all outputs of this tx
		for i, out := range tx.TxOut {
			if bytes.Equal(out.PkScript, aPKscript) { // new utxo for us
				var newu Utxo
				newu.AtHeight = height
				newu.KeyIdx = adr.KeyIdx
				newu.Value = out.Value
				var newop wire.OutPoint
				newop.Hash = tx.TxSha()
				newop.Index = uint32(i)
				newu.Op = newop
				b, err := newu.ToBytes()
				if err != nil {
					return hits, err
				}
				nUtxoBytes = append(nUtxoBytes, b)
				ts.Sum += newu.Value
				hits++
				break // only one match
			}

		}
	}

	err = ts.StateDB.Update(func(btx *bolt.Tx) error {
		// get all 4 buckets
		duf := btx.Bucket(BKTUtxos)
		//		sta := btx.Bucket(BKTState)
		//		old := btx.Bucket(BKTStxos)
		//		txns := btx.Bucket(BKTTxns)

		// first see if we lose utxos
		// iterate through duffel bag and look for matches
		// this makes us lose money, which is regrettable, but we need to know.
		for _, nOP := range spentOPs {
			duf.ForEach(func(k, v []byte) error {
				if bytes.Equal(k, nOP) { // matched, we lost utxo
					// do all this just to figure out value we lost
					x := make([]byte, len(k)+len(v))
					copy(x, k)
					copy(x[len(k):], v)
					lostTxo, err := UtxoFromBytes(x)
					if err != nil {
						return err
					}
					ts.Sum -= lostTxo.Value
					hits++
					// then delete the utxo from duf, save to old
					err = duf.Delete(k)
					if err != nil {
						return err
					}
					return nil // matched utxo k, won't match another
				}
				return nil // no match
			})
		} // done losing utxos
		// next add all new utxos to db, this is quick as the work is above
		for _, ub := range nUtxoBytes {
			err = duf.Put(ub[:36], ub[36:])
			if err != nil {
				return err
			}
		}
		return nil
	})
	return hits, err
}

// SaveToDB write a utxo to disk, overwriting an old utxo of the same outpoint
func (ts *TxStore) SaveUtxo(u *Utxo) error {
	b, err := u.ToBytes()
	if err != nil {
		return err
	}

	err = ts.StateDB.Update(func(btx *bolt.Tx) error {
		duf := btx.Bucket(BKTUtxos)
		sta := btx.Bucket(BKTState)
		// kindof hack, height is 36:40
		// also not really tip height...
		if u.AtHeight > 0 { // if confirmed
			err = sta.Put(KEYTipHeight, b[36:40])
			if err != nil {
				return err
			}
		}

		// key : val is txid:everything else
		return duf.Put(b[:36], b[36:])
	})
	if err != nil {
		return err
	}
	return nil
}

func (ts *TxStore) MarkSpent(ut Utxo, h int32, stx *wire.MsgTx) error {
	// we write in key = outpoint (32 hash, 4 index)
	// value = spending txid
	// if we care about the spending tx we can store that in another bucket.

	var st Stxo
	st.Utxo = ut
	st.SpendHeight = h
	st.SpendTxid = stx.TxSha()

	return ts.StateDB.Update(func(btx *bolt.Tx) error {
		duf := btx.Bucket(BKTUtxos)
		old := btx.Bucket(BKTStxos)
		txns := btx.Bucket(BKTTxns)

		opb, err := outPointToBytes(&st.Op)
		if err != nil {
			return err
		}

		err = duf.Delete(opb) // not utxo anymore
		if err != nil {
			return err
		}

		stxb, err := st.ToBytes()
		if err != nil {
			return err
		}

		err = old.Put(opb, stxb) // write k:v outpoint:stxo bytes
		if err != nil {
			return err
		}

		// store spending tx
		sha := stx.TxSha()
		var buf bytes.Buffer
		stx.Serialize(&buf)
		txns.Put(sha.Bytes(), buf.Bytes())

		return nil
	})
}

// LoadFromDB loads everything in the db file into ram, rebuilding the TxStore
// (except the rootPrivKey, that should be done before calling this --
// this will error if ts.rootPrivKey hasn't been loaded)
func (ts *TxStore) LoadFromDB() error {
	if ts.rootPrivKey == nil {
		return fmt.Errorf("LoadFromDB needs rootPrivKey loaded")
	}
	return ts.StateDB.View(func(btx *bolt.Tx) error {
		duf := btx.Bucket(BKTUtxos)
		if duf == nil {
			return fmt.Errorf("no duffel bag")
		}
		spent := btx.Bucket(BKTStxos)
		if spent == nil {
			return fmt.Errorf("no spenttx bucket")
		}
		sta := btx.Bucket(BKTState)
		if sta == nil {
			return fmt.Errorf("no state bucket")
		}
		// first populate addresses from state bucket
		numKeysBytes := sta.Get(KEYNumKeys)
		if numKeysBytes != nil { // NumKeys exists, read into uint32
			buf := bytes.NewBuffer(numKeysBytes)
			var numKeys uint32
			err := binary.Read(buf, binary.BigEndian, &numKeys)
			if err != nil {
				return err
			}
			fmt.Printf("db says %d keys\n", numKeys)
			err = ts.PopulateAdrs(numKeys)
			if err != nil {
				return err
			}
		}
		// next load all utxos from db into ram
		duf.ForEach(func(k, v []byte) error {
			// have to copy k and v here, otherwise append will crash it.
			// not quite sure why but append does weird stuff I guess.
			stx := spent.Get(k)
			if stx == nil { // if it's not in the spent bucket
				// create a new utxo
				x := make([]byte, len(k)+len(v))
				copy(x, k)
				copy(x[len(k):], v)
				newU, err := UtxoFromBytes(x)
				if err != nil {
					return err
				}
				// and add it to ram
				ts.Utxos = append(ts.Utxos, &newU)
				ts.Sum += newU.Value
			} else {
				fmt.Printf("had utxo %x but spent by tx %x...\n",
					k, stx[:8])
			}
			return nil
		})
		return nil
	})
}

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
