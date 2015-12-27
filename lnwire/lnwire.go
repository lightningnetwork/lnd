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

type CreateChannel struct {
	ChannelType uint8

	FundingAmount btcutil.Amount
	ReserveAmount btcutil.Amount
	MinFeePerKb   btcutil.Amount

	//Should double-check the total funding later
	MinTotalFundingAmount btcutil.Amount

	//CLTV lock-time to use
	LockTime uint32

	//Who pays the fees
	//0: (default) channel initiator
	//1: split
	//2: channel responder
	FeePayer uint8

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	DeliveryPkScript PkScript //*MUST* be either P2PKH or P2SH

	Inputs []*wire.TxIn
}

func (c *CreateChannel) String() string {

	var inputs string
	for i, in := range c.Inputs {
		inputs += fmt.Sprintf("\n     Slice\t%d\n", i)
		inputs += fmt.Sprintf("\tHash\t%s\n", in.PreviousOutPoint.Hash)
		inputs += fmt.Sprintf("\tIndex\t%d\n", in.PreviousOutPoint.Index)
	}
	return fmt.Sprintf("\n--- Begin CreateChannel ---\n") +
		fmt.Sprintf("ChannelType:\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("FundingAmount:\t\t%s\n", c.FundingAmount.String()) +
		fmt.Sprintf("ReserveAmount:\t\t%s\n", c.ReserveAmount.String()) +
		fmt.Sprintf("MinFeePerKb:\t\t%s\n", c.MinFeePerKb.String()) +
		fmt.Sprintf("MinTotalFundingAmount\t%s\n", c.MinTotalFundingAmount.String()) +
		fmt.Sprintf("LockTime\t\t%d\n", c.LockTime) +
		fmt.Sprintf("FeePayer\t\t%x\n", c.FeePayer) +
		fmt.Sprintf("RevocationHash\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t%x\n", c.Pubkey.SerializeCompressed()) +
		fmt.Sprintf("DeliveryPkScript\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("Inputs:") +
		inputs +
		fmt.Sprintf("--- End CreateChannel ---\n")
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
		err = binary.Write(w, binary.BigEndian, int64(e))
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
	case *uint8:
		var b [1]uint8
		_, err = r.Read(b[:])
		if err != nil {
			return err
		}
		*e = b[0]
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
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))
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
	case *[20]byte:
		_, err = io.ReadFull(r, e[:])
		if err != nil {
			return err
		}
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

			//Create TxIn
			txins = append(txins, wire.NewTxIn(outPoint, nil))
		}
		*e = *&txins

	default:
		return fmt.Errorf("Unknown type in readElement: %T", e)
	}

	return nil
}

func (c *CreateChannel) DeserializeFundingRequest(r io.Reader) error {
	var err error

	//Message Type (0/1)
	var messageType uint8
	err = readElement(r, &messageType)
	if err != nil {
		return err
	}
	if messageType != 0x30 {
		return fmt.Errorf("Message type does not match FundingRequest")
	}

	//Channel Type (1/1)
	err = readElement(r, &c.ChannelType)
	if err != nil {
		return err
	}

	// Funding Amount (2/8)
	err = readElement(r, &c.FundingAmount)
	if err != nil {
		return err
	}

	// Channel Minimum Capacity (10/8)
	err = readElement(r, &c.MinTotalFundingAmount)
	if err != nil {
		return err
	}

	// Revocation Hash (18/20)
	err = readElement(r, &c.RevocationHash)
	if err != nil {
		return err
	}

	// Commitment Pubkey (38/32)
	err = readElement(r, &c.Pubkey)
	if err != nil {
		return err
	}

	// Reserve Amount (70/8)
	err = readElement(r, &c.ReserveAmount)
	if err != nil {
		return err
	}

	//Minimum Transaction Fee Per Kb (78/8)
	err = readElement(r, &c.MinFeePerKb)
	if err != nil {
		return err
	}

	//LockTime (86/4)
	err = readElement(r, &c.LockTime)
	if err != nil {
		return err
	}

	//FeePayer (90/1)
	err = readElement(r, &c.FeePayer)
	if err != nil {
		return err
	}

	// Delivery PkScript
	err = readElement(r, &c.DeliveryPkScript)
	if err != nil {
		return err
	}

	//Create the TxIns
	err = readElement(r, &c.Inputs)
	if err != nil {
		return err
	}

	return nil
}

//Serializes the fundingRequest from the CreateChannel struct
//Writes the data to w
func (c *CreateChannel) SerializeFundingRequest(w io.Writer) error {
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

	//Funding Amont
	err = writeElement(w, c.FundingAmount)
	if err != nil {
		return err
	}

	// Channel Minimum Capacity
	err = writeElement(w, c.MinTotalFundingAmount)
	if err != nil {
		return err
	}

	// Revocation Hash
	err = writeElement(w, c.RevocationHash)
	if err != nil {
		return err
	}

	// Commitment Pubkey
	err = writeElement(w, c.Pubkey)
	if err != nil {
		return err
	}

	// Reserve Amount
	err = writeElement(w, c.ReserveAmount)
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

	// Delivery PkScript
	//First byte length then pkscript
	err = writeElement(w, c.DeliveryPkScript)
	if err != nil {
		return err
	}

	//Append the actual Txins
	//First byte is number of inputs
	//For each input, it's 32bytes txin & 4bytes index
	err = writeElement(w, c.Inputs)
	if err != nil {
		return err
	}

	return nil
}
