package uspv

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/boltdb/bolt"
)

var (
	BKTUtxos = []byte("DuffelBag")  // leave the rest to collect interest
	BKTOld   = []byte("SpentTxs")   // for bookkeeping
	KEYState = []byte("LastUpdate") // last state of DB
)

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
			if spent.Get(k) == nil { // if it's not in the spent bucket
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
	err = binary.Write(&buf, binary.BigEndian, u.Txo.Value)
	if err != nil {
		return nil, err
	}

	// write variable length (usually like 25 byte) pkscript
	_, err = buf.Write(u.Txo.PkScript)
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
	err = binary.Read(buf, binary.BigEndian, &u.Txo.Value)
	if err != nil {
		return u, err
	}
	// read variable length (usually like 25 byte) pkscript
	u.Txo.PkScript = buf.Bytes()
	return u, nil
}
