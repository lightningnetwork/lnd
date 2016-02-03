package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"

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
	var numKeys uint32
	ts.StateDB, err = bolt.Open(filename, 0644, nil)
	if err != nil {
		return err
	}
	// create buckets if they're not already there
	err = ts.StateDB.Update(func(btx *bolt.Tx) error {
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
		sta, err := btx.CreateBucketIfNotExists(BKTState)
		if err != nil {
			return err
		}

		numKeysBytes := sta.Get(KEYNumKeys)
		if numKeysBytes != nil { // NumKeys exists, read into uint32
			buf := bytes.NewBuffer(numKeysBytes)
			err := binary.Read(buf, binary.BigEndian, &numKeys)
			if err != nil {
				return err
			}
			fmt.Printf("db says %d keys\n", numKeys)
		} else { // no adrs yet, make it 1 (why...?)
			numKeys = 1
			var buf bytes.Buffer
			err = binary.Write(&buf, binary.BigEndian, numKeys)
			if err != nil {
				return err
			}
			err = sta.Put(KEYNumKeys, buf.Bytes())
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ts.PopulateAdrs(numKeys)
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
	var ma MyAdr
	ma.PkhAdr = newAdr
	ma.KeyIdx = n
	ts.Adrs = append(ts.Adrs, ma)

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
		var ma MyAdr
		ma.PkhAdr = newAdr
		ma.KeyIdx = k
		ts.Adrs = append(ts.Adrs, ma)

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

	// first check that we have a height and tx has been SPV OK'd
	inTxid := tx.TxSha()
	height, ok := ts.OKTxids[inTxid]
	if !ok {
		return hits, fmt.Errorf("Ingest error: tx %s not in OKTxids.",
			inTxid.String())
	}

	// tx has been OK'd by SPV; check tx sanity
	utilTx := btcutil.NewTx(tx) // convert for validation
	// checks stuff like inputs >= ouputs
	err = blockchain.CheckTransactionSanity(utilTx)
	if err != nil {
		return hits, err
	}
	// note that you can't check signatures; this is SPV.
	// 0 conf SPV means pretty much nothing.  Anyone can say anything.

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
		old := btx.Bucket(BKTStxos)
		txns := btx.Bucket(BKTTxns)

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
					// after deletion, save stxo to old bucket
					var st Stxo               // generate spent txo
					st.Utxo = lostTxo         // assign outpoint
					st.SpendHeight = height   // spent at height
					st.SpendTxid = tx.TxSha() // spent by txid
					stxb, err := st.ToBytes() // serialize
					if err != nil {
						return err
					}
					err = old.Put(k, stxb) // write k:v outpoint:stxo bytes
					if err != nil {
						return err
					}
					// store this relevant tx
					sha := tx.TxSha()
					var buf bytes.Buffer
					tx.Serialize(&buf)
					err = txns.Put(sha.Bytes(), buf.Bytes())
					if err != nil {
						return err
					}

					return nil // matched utxo k, won't match another
				}
				return nil // no match
			})
		} // done losing utxos, next gain utxos
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
