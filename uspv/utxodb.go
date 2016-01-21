package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"

	"github.com/btcsuite/btcd/wire"

	"github.com/boltdb/bolt"
)

var (
	BKTUtxos = []byte("DuffelBag")  // leave the rest to collect interest
	BKTOld   = []byte("SpentTxs")   // for bookkeeping
	KEYState = []byte("LastUpdate") // last state of DB
)

func (ts *TxStore) OpenDB(filename string) error {
	var err error
	ts.StateDB, err = bolt.Open(filename, 0644, nil)
	if err != nil {
		return err
	}
	// create buckets if they're not already there
	return ts.StateDB.Update(func(tx *bolt.Tx) error {
		_, err = tx.CreateBucketIfNotExists(BKTUtxos)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(BKTOld)
		if err != nil {
			return err
		}
		return nil
	})
}

func (ts *TxStore) PopulateAdrs(lastKey uint32) error {
	for k := uint32(0); k < lastKey; k++ {

		priv, err := ts.rootPrivKey.Child(k)
		if err != nil {
			log.Fatal(err)
		}
		myadr, err := priv.Address(ts.param)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("made adr %s\n", myadr.String())
		ts.AddAdr(myadr, k)
	}
	return nil
}
func (u *Utxo) SaveToDB(dbx *bolt.DB) error {
	return dbx.Update(func(tx *bolt.Tx) error {
		duf := tx.Bucket(BKTUtxos)
		b, err := u.ToBytes()
		if err != nil {
			return err
		}
		// key : val is txid:everything else
		return duf.Put(b[:36], b[36:])
	})
}

func (ts *TxStore) MarkSpent(op *wire.OutPoint, h int32, stx *wire.MsgTx) error {

	// we write in key = outpoint (32 hash, 4 index)
	// value = spending txid
	// if we care about the spending tx we can store that in another bucket.
	return ts.StateDB.Update(func(tx *bolt.Tx) error {
		old := tx.Bucket(BKTOld)
		opb, err := outPointToBytes(op)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		err = binary.Write(&buf, binary.BigEndian, h)
		if err != nil {
			return err
		}
		sha := stx.TxSha()
		err = old.Put(opb, sha.Bytes()) // write k:v outpoint:txid
		if err != nil {
			return err
		}
		return nil
	})
}

func (ts *TxStore) LoadUtxos() error {
	err := ts.StateDB.View(func(tx *bolt.Tx) error {
		duf := tx.Bucket(BKTUtxos)
		if duf == nil {
			return fmt.Errorf("no duffel bag")
		}
		spent := tx.Bucket(BKTOld)
		if spent == nil {
			return fmt.Errorf("no spenttx bucket")
		}

		duf.ForEach(func(k, v []byte) error {
			// have to copy these here, otherwise append will crash it.
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
				ts.Utxos = append(ts.Utxos, newU)
			} else {
				fmt.Printf("had utxo %x but spent by tx %x...\n",
					k, stx[:8])
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return err
	}
	return nil
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
	if buf.Len() < 52 { // minimum 52 bytes with no pkscript
		return u, fmt.Errorf("Got %d bytes for sender, expect > 52", buf.Len())
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
