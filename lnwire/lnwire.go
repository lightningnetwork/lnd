package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"io"
)

//Actual pkScript, not redeemScript
type PkScript []byte

//Subsatoshi amount
type MicroSatoshi int32

type CreateChannel struct {
	ChannelType uint8

	OurFundingAmount   btcutil.Amount
	TheirFundingAmount btcutil.Amount
	OurReserveAmount   btcutil.Amount
	TheirReserveAmount btcutil.Amount
	OurMinFeePerKb     btcutil.Amount
	TheirMinFeePerKb   btcutil.Amount

	//Either party can change.
	//Should double-check the total funding later
	MinTotalFundingAmount btcutil.Amount

	//CLTV lock-time to use
	LockTime uint32

	//Who pays the fees
	//0: (default) channel initiator
	//1: split
	//2: channel responder
	FeePayer uint8

	OurRevocationHash     [20]byte
	TheirRevocationHash   [20]byte
	OurPubkey             *btcec.PublicKey
	TheirPubkey           *btcec.PublicKey
	OurDeliveryPkScript   PkScript //*MUST* be either P2PKH or P2SH
	TheirDeliveryPkScript PkScript //*MUST* be either P2PKH or P2SH

	OurInputs   []*wire.TxIn
	TheirInputs []*wire.TxIn
}

//Writes the big endian representation of element
//Unified function to call when writing different types
//Pre-allocate a byte-array of the correct size for cargo-cult security
//More copies but whatever...
func writeElement(w *bytes.Buffer, element interface{}) error {
	var err error
	switch e := element.(type) {
	case uint8:
		var b [1]byte
		b[0] = byte(e)
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
	case uint32:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
	case btcutil.Amount:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		_, err = w.Write(b[:])
		if err != nil {
			return err
		}
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
	case [20]byte:
		_, err = w.Write(e[:])
		if err != nil {
			return err
		}
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
		}
	default:
		return fmt.Errorf("Unknown type in writeElement: %T", e)
	}

	return nil
}

func readElement(r *bytes.Buffer, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *uint8:
		err = binary.Read(r, binary.BigEndian, *e)
		if err != nil {
			return err
		}
	case *uint32:
		var b [4]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = binary.BigEndian.Uint32(b[:])
	case *btcutil.Amount:
		var b [8]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = btcutil.Amount(binary.BigEndian.Uint64(b[:]))
	case *btcec.PublicKey:
		var b [33]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		e, err = btcec.ParsePubKey(b[:], btcec.S256())
		if err != nil {
			return err
		}
	case *[20]byte:
		_, err = io.ReadFull(r, e[:])
		if err != nil {
			return err
		}
	case *PkScript:
		//Get the script length first
		var scriptLength uint8
		err = binary.Read(r, binary.BigEndian, scriptLength)
		if err != nil {
			return err
		}
		if scriptLength > 25 {
			return fmt.Errorf("PkScript too long!")
		}

		//Read the script length
		l := io.LimitReader(r, int64(scriptLength))
		if err != nil {
			return err
		}
		l.Read(*e)
	case []*wire.TxIn:
		//Read the size (1-byte number of txins)
		var numScripts uint8
		err = binary.Read(r, binary.BigEndian, numScripts)
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

			//Create TxIn
			txins = append(txins, wire.NewTxIn(outPoint, nil))
		}
		e = txins

	default:
		return fmt.Errorf("Unknown type in readElement: %T", e)
	}

	return nil
}

func (c *CreateChannel) DeserializeFundingRequest(r *bytes.Buffer) error {
	var err error

	//Message Type
	var messageType uint8
	err = readElement(r, messageType)
	if err != nil {
		return err
	}
	if messageType != 0x30 {
		return fmt.Errorf("Message type does not match FundingRequest")
	}

	//Channel Type
	err = readElement(r, c.ChannelType)
	if err != nil {
		return err
	}

	//Their Funding Amount
	err = readElement(r, c.TheirFundingAmount)
	if err != nil {
		return err
	}

	//Their Channel Minimum Capacity
	var theirMinimumFunding btcutil.Amount
	err = readElement(r, theirMinimumFunding)
	if err != nil {
		return err
	}
	//Replace with their minimum if it's greater than ours
	//We still need to locally validate that info
	if theirMinimumFunding > c.MinTotalFundingAmount {
		c.MinTotalFundingAmount = theirMinimumFunding
	}

	//Their Revocation Hash
	err = readElement(r, c.TheirRevocationHash)
	if err != nil {
		return err
	}

	//Their Commitment Pubkey
	err = readElement(r, c.TheirPubkey)
	if err != nil {
		return err
	}

	//Their Reserve Amount
	err = readElement(r, c.TheirReserveAmount)
	if err != nil {
		return err
	}

	//Minimum Transaction Fee Per Kb
	err = readElement(r, c.TheirMinFeePerKb)
	if err != nil {
		return err
	}

	//LockTime
	err = readElement(r, c.LockTime)
	if err != nil {
		return err
	}

	//FeePayer
	err = readElement(r, c.FeePayer)
	if err != nil {
		return err
	}

	//Their Delivery PkScript
	err = readElement(r, c.TheirDeliveryPkScript)
	if err != nil {
		return err
	}

	//Create the TxIns
	err = readElement(r, c.TheirInputs)
	if err != nil {
		return err
	}

	return nil
}

//Serializes the fundingRequest from the CreateChannel struct
//Writes the data to w
func (c *CreateChannel) SerializeFundingRequest(w *bytes.Buffer) error {
	var err error

	//Fund request byte
	err = writeElement(w, uint8(0x30))
	if err != nil {
		return err
	}

	//Channel Type
	//default to 0 for CLTV-only
	err = writeElement(w, c.ChannelType)
	if err != nil {
		return err
	}

	//Our Funding Amont
	err = writeElement(w, c.OurFundingAmount)
	if err != nil {
		return err
	}

	//Our Channel Minimum Capacity
	err = writeElement(w, c.MinTotalFundingAmount)
	if err != nil {
		return err
	}

	//Our Revocation Hash
	err = writeElement(w, c.OurRevocationHash)
	if err != nil {
		return err
	}

	//Our Commitment Pubkey
	err = writeElement(w, c.OurPubkey)
	if err != nil {
		return err
	}

	//Our Reserve Amount
	err = writeElement(w, c.OurReserveAmount)
	if err != nil {
		return err
	}

	//Minimum Transaction Fee Per KB
	err = writeElement(w, c.OurMinFeePerKb)
	if err != nil {
		return err
	}

	//LockTime
	err = writeElement(w, c.LockTime)
	if err != nil {
		return err
	}

	//FeePayer
	err = writeElement(w, c.FeePayer)
	if err != nil {
		return err
	}

	//Our Delivery PkScript
	//First byte length then pkscript
	err = writeElement(w, c.OurDeliveryPkScript)
	if err != nil {
		return err
	}

	//Append the actual Txins
	//First byte is number of inputs
	//For each input, it's 32bytes txin & 4bytes index
	err = writeElement(w, c.OurInputs)
	if err != nil {
		return err
	}

	return nil
}
