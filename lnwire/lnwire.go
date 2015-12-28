package lnwire

import (
	"encoding/binary"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"io"
	"io/ioutil"
)

//Actual pkScript, not redeemScript
type PkScript []byte

//Subsatoshi amount
type MicroSatoshi int32

//Writes the big endian representation of element
//Unified function to call when writing different types
//Pre-allocate a byte-array of the correct size for cargo-cult security
//More copies but whatever...
func writeElement(w io.Writer, includeSig bool, element interface{}) error {
	var err error
	switch e := element.(type) {
	case uint8:
		var b [1]byte
		b[0] = byte(e)
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
		return nil
	case uint32:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
		return nil
	case btcutil.Amount:
		err = binary.Write(w, binary.BigEndian, int64(e))
		if err != nil {
			return err
		}
		return nil
	case *btcec.PublicKey:
		var b [33]byte
		serializedPubkey := e.SerializeCompressed()
		if len(serializedPubkey) != 33 {
			return fmt.Errorf("Wrong size pubkey")
		}
		copy(b[:], serializedPubkey)
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
		return nil
	case [20]byte:
		_, err = w.Write(e[:])
		if err != nil {
			return err
		}
		return nil
	case wire.BitcoinNet:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		_, err := w.Write(b[:])
		if err != nil {
			return err
		}
		return nil
	case PkScript:
		scriptLength := len(e)
		//Make sure it's P2PKH or P2SH size or less
		if scriptLength > 25 {
			return fmt.Errorf("PkScript too long!")
		}
		//Write the size (1-byte)
		err = binary.Write(w, binary.BigEndian, uint8(scriptLength))
		if err != nil {
			return err
		}
		//Write the data
		_, err = w.Write(e)
		if err != nil {
			return err
		}
		return nil
	case []*wire.TxIn:
		//Append the unsigned(!!!) txins
		//Write the size (1-byte)
		if len(e) > 127 {
			return fmt.Errorf("Too many txins")
		}
		err = binary.Write(w, binary.BigEndian, uint8(len(e)))
		if err != nil {
			return err
		}
		//Append the actual TxIns (Size: NumOfTxins * 36)
		//Do not include the sequence number to eliminate funny business
		for _, in := range e {
			//Hash
			var h [32]byte
			copy(h[:], in.PreviousOutPoint.Hash.Bytes())
			_, err = w.Write(h[:])
			if err != nil {
				return err
			}
			//Index
			var idx [4]byte
			binary.BigEndian.PutUint32(idx[:], in.PreviousOutPoint.Index)
			_, err = w.Write(idx[:])
			if err != nil {
				return err
			}
			//Signature(optional)
			if includeSig {
				var sig [33]byte
				copy(sig[:], in.SignatureScript)
				_, err = w.Write(sig[:])
				if err != nil {
					return err
				}

			}
		}
		return nil
	default:
		return fmt.Errorf("Unknown type in writeElement: %T", e)
	}

	return nil
}

func writeElements(w io.Writer, includeSig bool, elements ...interface{}) error {
	for _, element := range elements {
		err := writeElement(w, includeSig, element)
		if err != nil {
			return err
		}
	}
	return nil
}

func readElement(r io.Reader, includeSig bool, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *uint8:
		var b [1]uint8
		_, err = r.Read(b[:])
		if err != nil {
			return err
		}
		*e = b[0]
		return nil
	case *uint32:
		var b [4]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = binary.BigEndian.Uint32(b[:])
		return nil
	case *btcutil.Amount:
		var b [8]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))
		return nil
	case **btcec.PublicKey:
		var b [33]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		x, err := btcec.ParsePubKey(b[:], btcec.S256())
		*e = &*x
		if err != nil {
			return err
		}
		return nil
	case *[20]byte:
		_, err = io.ReadFull(r, e[:])
		if err != nil {
			return err
		}
		return nil
	case *wire.BitcoinNet:
		var b [4]byte
		_, err := io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = wire.BitcoinNet(binary.BigEndian.Uint32(b[:]))
		return nil
	case *PkScript:
		//Get the script length first
		var scriptLength [1]uint8
		_, err = r.Read(scriptLength[:])
		if err != nil {
			return err
		}

		if scriptLength[0] > 25 {
			return fmt.Errorf("PkScript too long!")
		}

		//Read the script length
		l := io.LimitReader(r, int64(scriptLength[0]))
		*e, _ = ioutil.ReadAll(l)
		if err != nil {
			return err
		}
		return nil
	case *[]*wire.TxIn:
		//Read the size (1-byte number of txins)
		var numScripts [1]uint8
		_, err = r.Read(numScripts[:])
		if err != nil {
			return err
		}
		if numScripts[0] > 127 {
			return fmt.Errorf("Too many txins")
		}

		//Append the actual TxIns
		var txins []*wire.TxIn
		for i := uint8(0); i < numScripts[0]; i++ {
			//Hash
			var h [32]byte
			_, err = io.ReadFull(r, h[:])
			if err != nil {
				return err
			}
			shaHash, err := wire.NewShaHash(h[:])
			if err != nil {
				return err
			}
			//Index
			var idxBytes [4]byte
			_, err = io.ReadFull(r, idxBytes[:])
			if err != nil {
				return err
			}
			index := binary.BigEndian.Uint32(idxBytes[:])
			outPoint := wire.NewOutPoint(shaHash, index)
			//Signature(optional)
			if includeSig {
				var sig [33]byte
				_, err = io.ReadFull(r, sig[:])
				if err != nil {
					return err
				}
				//Create TxIn
				txins = append(txins, wire.NewTxIn(outPoint, sig[:]))
			} else { //no signature
				//Create TxIn
				txins = append(txins, wire.NewTxIn(outPoint, nil))
			}

		}
		*e = *&txins
		return nil
	default:
		return fmt.Errorf("Unknown type in readElement: %T", e)
	}

	return nil
}

func readElements(r io.Reader, includeSig bool, elements ...interface{}) error {
	for _, element := range elements {
		err := readElement(r, includeSig, element)
		if err != nil {
			return err
		}
	}
	return nil
}
