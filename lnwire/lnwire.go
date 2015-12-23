package lnwire

import (
	"encoding/binary"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"io"
)

//Actual pkScript, not redeemScript
type PkScript []byte

type CreateChannel struct {
	ChannelType           uint8
	OurFundingAmount      btcutil.Amount
	OurReserveAmount      btcutil.Amount
	MinFeePerKb           btcutil.Amount //higher of both parties
	MinTotalFundingAmount btcutil.Amount

	//CLTV lock-time to use
	LockTime uint32

	//Who pays the fees
	//0: (default) channel initiator
	//1: split
	//2: channel responder
	FeePayer uint8

	OurRevocationHash   [20]byte
	TheirRevocationHash [20]byte
	OurPubkey           *btcec.PublicKey
	TheirPubkey         *btcec.PublicKey
	DeliveryPkScript    PkScript //*MUST* be either P2PKH or P2SH

	OurInputs   []*wire.TxIn
	TheirInputs []*wire.TxIn
}

//Writes the big endian representation of element
//Unified function to call when writing different types
//Pre-allocate a byte-array of the correct size for cargo-cult security
//More copies but whatever...
func writeElement(w io.Writer, element interface{}) error {
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

func readElement(r io.Reader, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *uint32:
		var b [4]byte
		_, err = io.ReadFull(r, b[:])
		if err != nil {
			return err
		}
		*e = binary.BigEndian.Uint32(b[:])
	}

	return nil
}

//Serializes the fundingRequest from the CreateChannel struct
//Writes the data to w
func (c *CreateChannel) SerializeFundingRequest(w io.Writer) error {
	var err error

	//Fund request byte
	err = binary.Write(w, binary.BigEndian, uint8(0x30))
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

	//Our Delivery PkHash
	err = writeElement(w, c.OurRevocationHash)
	if err != nil {
		return err
	}

	//Our Reserve Amount
	err = writeElement(w, c.OurReserveAmount)
	if err != nil {
		return err
	}

	//Minimum Transaction Fee Per KB
	err = writeElement(w, c.MinFeePerKb)
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

	//Delivery PkScript
	err = writeElement(w, c.DeliveryPkScript)
	if err != nil {
		return err
	}

	//Append the actual Txins
	err = writeElement(w, c.OurInputs)
	if err != nil {
		return err
	}

	return nil
}
