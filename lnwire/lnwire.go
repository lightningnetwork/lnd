package lnwire

import (
	"bytes"
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
	case uint64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
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
	case *[]btcec.Signature:
		numSigs := len(*e)
		if numSigs > 127 {
			return fmt.Errorf("Too many signatures!")
		}
		//Write the size
		err = writeElement(w, false, uint8(numSigs))
		if err != nil {
			return err
		}
		//Write the data
		for i := 0; i < numSigs; i++ {
			err = writeElement(w, false, &(*e)[i])
			if err != nil {
				return err
			}
		}
		return nil
	case *btcec.Signature:
		sig := e.Serialize()
		sigLength := len(sig)
		if sigLength > 73 {
			return fmt.Errorf("Signature too long!")
		}
		//Write the size
		err = writeElement(w, false, uint8(sigLength))
		if err != nil {
			return err
		}
		//Write the data
		_, err = w.Write(sig)
		if err != nil {
			return err
		}
		return nil
	case *wire.ShaHash:
		_, err = w.Write(e[:])
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
		err = writeElement(w, false, uint8(scriptLength))
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
		err = writeElement(w, false, uint8(len(e)))
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
	case *uint64:
		var b [8]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = binary.BigEndian.Uint64(b[:])
		return nil
	case *btcutil.Amount:
		var b [8]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))
		return nil
	case **wire.ShaHash:
		var b wire.ShaHash
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = &b
		return nil
	case **btcec.PublicKey:
		var b [33]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		x, err := btcec.ParsePubKey(b[:], btcec.S256())
		if err != nil {
			return err
		}
		*e = &*x
		return nil
	case **[]btcec.Signature:
		var numSigs uint8
		err = readElement(r, false, &numSigs)
		if err != nil {
			return err
		}
		if numSigs > 127 {
			return fmt.Errorf("Too many signatures!")
		}

		//Read that number of signatures
		var sigs []btcec.Signature
		for i := uint8(0); i < numSigs; i++ {
			sig := new(btcec.Signature)
			readElement(r, false, &sig)
			sigs = append(sigs, *sig)
		}
		*e = &sigs
		return nil
	case **btcec.Signature:
		var sigLength uint8
		err = readElement(r, false, &sigLength)
		if err != nil {
			return err
		}

		if sigLength > 73 {
			return fmt.Errorf("Signature too long!")
		}

		//Read the sig length
		l := io.LimitReader(r, int64(sigLength))
		sig, err := ioutil.ReadAll(l)
		if err != nil {
			return err
		}
		if len(sig) != int(sigLength) {
			return fmt.Errorf("EOF: Signature length mismatch.")
		}
		btcecSig, err := btcec.ParseSignature(sig, btcec.S256())
		if err != nil {
			return err
		}
		*e = &*btcecSig
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
		var scriptLength uint8
		err = readElement(r, false, &scriptLength)
		if err != nil {
			return err
		}

		if scriptLength > 25 {
			return fmt.Errorf("PkScript too long!")
		}

		//Read the script length
		l := io.LimitReader(r, int64(scriptLength))
		*e, err = ioutil.ReadAll(l)
		if len(*e) != int(scriptLength) {
			return fmt.Errorf("EOF: Signature length mismatch.")
		}
		if err != nil {
			return err
		}
		return nil
	case *[]*wire.TxIn:
		//Read the size (1-byte number of txins)
		var numScripts uint8
		err = readElement(r, false, &numScripts)
		if err != nil {
			return err
		}
		if numScripts > 127 {
			return fmt.Errorf("Too many txins")
		}

		//Append the actual TxIns
		var txins []*wire.TxIn
		for i := uint8(0); i < numScripts; i++ {
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

//Validates whether a PkScript byte array is P2SH or P2PKH
func ValidatePkScript(pkScript PkScript) error {
	if len(pkScript) == 25 {
		//P2PKH
		//Begins with OP_DUP OP_HASH160 PUSHDATA(20)
		if !bytes.Equal(pkScript[0:3], []byte{118, 169, 20}) ||
			//Ends with OP_EQUALVERIFY OP_CHECKSIG
			!bytes.Equal(pkScript[23:25], []byte{136, 172}) {
			//If it's not correct, return error
			return fmt.Errorf("PkScript only allows P2SH or P2PKH")
		}
	} else if len(pkScript) == 23 {
		//P2SH
		//Begins with OP_HASH160 PUSHDATA(20)
		if !bytes.Equal(pkScript[0:2], []byte{169, 20}) ||
			//Ends with OP_EQUAL
			!bytes.Equal(pkScript[22:23], []byte{135}) {
			//If it's not correct, return error
			return fmt.Errorf("PkScript only allows P2SH or P2PKH")
		}
	} else {
		//Length not 23 or 25
		return fmt.Errorf("PkScript only allows P2SH or P2PKH")
	}

	return nil
}
