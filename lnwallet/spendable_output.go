package lnwallet

import (
	"encoding/binary"
	"io"

	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var byteOrder = binary.BigEndian

// BaseOutput is base implementation of SpendableOutput interface
type BaseOutput struct {
	amt         btcutil.Amount
	outpoint    wire.OutPoint
	witnessType WitnessType
	signDesc    SignDescriptor

	witnessFunc WitnessGenerator
}

// NewBaseOutput creates new base output instance
func NewBaseOutput(
	amt btcutil.Amount,
	outpoint wire.OutPoint,
	witnessType WitnessType,
	signDesc SignDescriptor,
) *BaseOutput {
	return &BaseOutput{
		amt: amt,
		outpoint: outpoint,
		witnessType: witnessType,
		signDesc: signDesc,
	}
}

// Amount returns amount of the output.
func (s *BaseOutput) Amount() btcutil.Amount {
	return s.amt
}

// OutPoint is previous transaction output.
func (s *BaseOutput) OutPoint() *wire.OutPoint {
	return &s.outpoint
}

// WitnessType returns types which attached to generation of witness data.
func (s *BaseOutput) WitnessType() WitnessType {
	return s.witnessType
}

// SignDesc is used to signing raw transaction.
func (s *BaseOutput) SignDesc() *SignDescriptor {
	return &s.signDesc
}

// BuildWitness computes a valid witness that allows us to spend from the
// output. It does so by first generating and memoizing the witness
// generation function, which parameterized primarily by the witness type and
// sign descriptor. The method then returns the witness computed by invoking
// this function on the first and subsequent calls.
func (s *BaseOutput) BuildWitness(signer Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) ([][]byte, error) {
	// Now that we have ensured that the witness generation function has
	// been initialized, we can proceed to execute it and generate the
	// witness for this particular breached output.
	s.witnessFunc = s.witnessType.GenWitnessFunc(
		signer, s.SignDesc(),
	)

	return s.witnessFunc(txn, hashCache, txinIdx)
}

// Encode serializes data of spendable output to serial data
func (s *BaseOutput) Encode(w io.Writer) error {
	var scratch [8]byte

	byteOrder.PutUint64(scratch[:], uint64(s.Amount()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, s.OutPoint()); err != nil {
		return err
	}

	byteOrder.PutUint16(scratch[:2], uint16(s.WitnessType()))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	if err := WriteSignDescriptor(w, s.SignDesc()); err != nil {
		return err
	}

	return nil
}

// Decode deserializes data of spendable output from serial data
func (s *BaseOutput) Decode(r io.Reader) error {
	var (
		scratch [8]byte
		err     error
	)

	if _, err = r.Read(scratch[:]); err != nil && err != io.EOF {
		return err
	}
	s.amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if err = readOutpoint(io.LimitReader(r, 40),
		&s.outpoint); err != nil && err != io.EOF {
		return err
	}

	if _, err = r.Read(scratch[:2]); err != nil && err != io.EOF {
		return err
	}
	s.witnessType = WitnessType(
		byteOrder.Uint16(scratch[:2]),
	)

	if err = ReadSignDescriptor(r, &s.signDesc);
		err != nil && err != io.EOF {
		return err
	}

	if err != nil && err != io.EOF {
		return err
	}

	return nil
}

// writeOutpoint writes an outpoint to the passed writer using the minimal
// amount of bytes possible.
func writeOutpoint(w io.Writer, o *wire.OutPoint) error {
	if _, err := w.Write(o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, o.Index); err != nil {
		return err
	}

	return nil
}

// readOutpoint reads an outpoint from the passed reader that was previously
// written using the writeOutpoint struct.
func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Read(r, byteOrder, &o.Index); err != nil {
		return err
	}

	return nil
}

// Add compile-time constraint ensuring BaseOutput implements
// SpendableOutput.
var _ SpendableOutput = (*BaseOutput)(nil)