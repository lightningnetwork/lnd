package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/roasbeef/btcd/blockchain"

	"github.com/roasbeef/btcd/txscript"

	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/hdkeychain"

	"github.com/boltdb/bolt"
)

var (
	BKTUtxos = []byte("DuffelBag") // leave the rest to collect interest
	BKTStxos = []byte("SpentTxs")  // for bookkeeping
	BKTTxns  = []byte("Txns")      // all txs we care about, for replays
	BKTState = []byte("MiscState") // last state of DB
	// these are in the state bucket
	KEYNumKeys   = []byte("NumKeys")   // number of p2pkh keys used
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
func (ts *TxStore) NewAdr() (btcutil.Address, error) {
	if ts.Param == nil {
		return nil, fmt.Errorf("NewAdr error: nil param")
	}

	priv := new(hdkeychain.ExtendedKey)
	var err error
	var n uint32
	var nAdr btcutil.Address

	n = uint32(len(ts.Adrs))
	priv, err = ts.rootPrivKey.Child(n + hdkeychain.HardenedKeyStart)
	if err != nil {
		return nil, err
	}
	nAdr, err = priv.Address(ts.Param)
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
	ma.PkhAdr = nAdr
	ma.KeyIdx = n

	ts.Adrs = append(ts.Adrs, ma)
	ts.localFilter.Add(ma.PkhAdr.ScriptAddress())
	return nAdr, nil
}

// SetDBSyncHeight sets sync height of the db, indicated the latest block
// of which it has ingested all the transactions.
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

// GetAllUtxos returns a slice of all utxos known to the db. empty slice is OK.
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

// GetAllStxos returns a slice of all stxos known to the db. empty slice is OK.
func (ts *TxStore) GetAllStxos() ([]*Stxo, error) {
	// this is almost the same as GetAllUtxos but whatever, it'd be more
	// complicated to make one contain the other or something
	var stxos []*Stxo
	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		old := btx.Bucket(BKTStxos)
		if old == nil {
			return fmt.Errorf("no old txos")
		}
		return old.ForEach(func(k, v []byte) error {
			// have to copy k and v here, otherwise append will crash it.
			// not quite sure why but append does weird stuff I guess.

			// create a new stxo
			x := make([]byte, len(k)+len(v))
			copy(x, k)
			copy(x[len(k):], v)
			newS, err := StxoFromBytes(x)
			if err != nil {
				return err
			}
			// and add it to ram
			stxos = append(stxos, &newS)
			return nil
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stxos, nil
}

// GetTx takes a txid and returns the transaction.  If we have it.
func (ts *TxStore) GetTx(txid *wire.ShaHash) (*wire.MsgTx, error) {
	rtx := wire.NewMsgTx()

	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		txns := btx.Bucket(BKTTxns)
		if txns == nil {
			return fmt.Errorf("no transactions in db")
		}
		txbytes := txns.Get(txid.Bytes())
		if txbytes == nil {
			return fmt.Errorf("tx %x not in db", txid.String())
		}
		buf := bytes.NewBuffer(txbytes)
		return rtx.Deserialize(buf)
	})
	if err != nil {
		return nil, err
	}
	return rtx, nil
}

// GetTx takes a txid and returns the transaction.  If we have it.
func (ts *TxStore) GetAllTxs() ([]*wire.MsgTx, error) {
	var rtxs []*wire.MsgTx

	err := ts.StateDB.View(func(btx *bolt.Tx) error {
		txns := btx.Bucket(BKTTxns)
		if txns == nil {
			return fmt.Errorf("no transactions in db")
		}

		return txns.ForEach(func(k, v []byte) error {
			tx := wire.NewMsgTx()
			buf := bytes.NewBuffer(v)
			err := tx.Deserialize(buf)
			if err != nil {
				return err
			}
			rtxs = append(rtxs, tx)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return rtxs, nil
}

// GetPendingInv returns an inv message containing all txs known to the
// db which are at height 0 (not known to be confirmed).
// This can be useful on startup or to rebroadcast unconfirmed txs.
func (ts *TxStore) GetPendingInv() (*wire.MsgInv, error) {
	// use a map (really a set) do avoid dupes
	txidMap := make(map[wire.ShaHash]struct{})

	utxos, err := ts.GetAllUtxos() // get utxos from db
	if err != nil {
		return nil, err
	}
	stxos, err := ts.GetAllStxos() // get stxos from db
	if err != nil {
		return nil, err
	}

	// iterate through utxos, adding txids of anything with height 0
	for _, utxo := range utxos {
		if utxo.AtHeight == 0 {
			txidMap[utxo.Op.Hash] = struct{}{} // adds to map
		}
	}
	// do the same with stxos based on height at which spent
	for _, stxo := range stxos {
		if stxo.SpendHeight == 0 {
			txidMap[stxo.SpendTxid] = struct{}{}
		}
	}

	invMsg := wire.NewMsgInv()
	for txid := range txidMap {
		item := wire.NewInvVect(wire.InvTypeTx, &txid)
		err = invMsg.AddInvVect(item)
		if err != nil {
			if err != nil {
				return nil, err
			}
		}
	}

	// return inv message with all txids (maybe none)
	return invMsg, nil
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
func (ts *TxStore) Ingest(tx *wire.MsgTx, height int32) (uint32, error) {
	var hits uint32
	var err error
	var nUtxoBytes [][]byte

	// tx has been OK'd by SPV; check tx sanity
	utilTx := btcutil.NewTx(tx) // convert for validation
	// checks basic stuff like there are inputs and ouputs
	err = blockchain.CheckTransactionSanity(utilTx)
	if err != nil {
		return hits, err
	}
	// note that you can't check signatures; this is SPV.
	// 0 conf SPV means pretty much nothing.  Anyone can say anything.

	spentOPs := make([][]byte, len(tx.TxIn))
	// before entering into db, serialize all inputs of the ingested tx
	for i, txin := range tx.TxIn {
		spentOPs[i], err = outPointToBytes(&txin.PreviousOutPoint)
		if err != nil {
			return hits, err
		}
	}

	// go through txouts, and then go through addresses to match

	// generate PKscripts for all addresses
	wPKscripts := make([][]byte, len(ts.Adrs))
	aPKscripts := make([][]byte, len(ts.Adrs))

	for i, _ := range ts.Adrs {
		// iterate through all our addresses
		// convert regular address to witness address.  (split adrs later)
		wa, err := btcutil.NewAddressWitnessPubKeyHash(
			ts.Adrs[i].PkhAdr.ScriptAddress(), ts.Param)
		if err != nil {
			return hits, err
		}

		wPKscripts[i], err = txscript.PayToAddrScript(wa)
		if err != nil {
			return hits, err
		}
		aPKscripts[i], err = txscript.PayToAddrScript(ts.Adrs[i].PkhAdr)
		if err != nil {
			return hits, err
		}
	}

	cachedSha := tx.TxSha()
	// iterate through all outputs of this tx, see if we gain
	for i, out := range tx.TxOut {
		for j, ascr := range aPKscripts {
			// detect p2wpkh
			witBool := false
			if bytes.Equal(out.PkScript, wPKscripts[j]) {
				witBool = true
			}
			if bytes.Equal(out.PkScript, ascr) || witBool { // new utxo found
				var newu Utxo // create new utxo and copy into it
				newu.AtHeight = height
				newu.KeyIdx = ts.Adrs[j].KeyIdx
				newu.Value = out.Value
				newu.IsWit = witBool // copy witness version from pkscript
				var newop wire.OutPoint
				newop.Hash = cachedSha
				newop.Index = uint32(i)
				newu.Op = newop
				b, err := newu.ToBytes()
				if err != nil {
					return hits, err
				}
				nUtxoBytes = append(nUtxoBytes, b)
				hits++
				break // txos can match only 1 script
			}
		}
	}

	err = ts.StateDB.Update(func(btx *bolt.Tx) error {
		// get all 4 buckets
		duf := btx.Bucket(BKTUtxos)
		//		sta := btx.Bucket(BKTState)
		old := btx.Bucket(BKTStxos)
		txns := btx.Bucket(BKTTxns)
		if duf == nil || old == nil || txns == nil {
			return fmt.Errorf("error: db not initialized")
		}

		// iterate through duffel bag and look for matches
		// this makes us lose money, which is regrettable, but we need to know.
		for _, nOP := range spentOPs {
			v := duf.Get(nOP)
			if v != nil {
				hits++
				// do all this just to figure out value we lost
				x := make([]byte, len(nOP)+len(v))
				copy(x, nOP)
				copy(x[len(nOP):], v)
				lostTxo, err := UtxoFromBytes(x)
				if err != nil {
					return err
				}

				// after marking for deletion, save stxo to old bucket
				var st Stxo               // generate spent txo
				st.Utxo = lostTxo         // assign outpoint
				st.SpendHeight = height   // spent at height
				st.SpendTxid = cachedSha  // spent by txid
				stxb, err := st.ToBytes() // serialize
				if err != nil {
					return err
				}
				err = old.Put(nOP, stxb) // write nOP:v outpoint:stxo bytes
				if err != nil {
					return err
				}

				err = duf.Delete(nOP)
				if err != nil {
					return err
				}
			}
		}

		// done losing utxos, next gain utxos
		// next add all new utxos to db, this is quick as the work is above
		for _, ub := range nUtxoBytes {
			err = duf.Put(ub[:36], ub[36:])
			if err != nil {
				return err
			}
		}

		// if hits is nonzero it's a relevant tx and we should store it
		var buf bytes.Buffer
		tx.Serialize(&buf)
		err = txns.Put(cachedSha.Bytes(), buf.Bytes())
		if err != nil {
			return err
		}

		return nil
	})
	return hits, err
}
