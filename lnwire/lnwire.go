package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"net"

	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// MaxSliceLength is the maximum allowed lenth for any opaque byte slices in
// the wire protocol.
const MaxSliceLength = 65535

// PkScript is simple type definition which represents a raw serialized public
// key script.
type PkScript []byte

// HTLCKey is an identifier used to uniquely identify any HTLCs transmitted
// between Alice and Bob. In order to cancel, timeout, or settle HTLCs this
// identifier should be used to allow either side to easily locate and modify
// any staged or pending HTLCs.
// TODO(roasbeef): change to HTLCIdentifier?
type HTLCKey int64

// CommitHeight is an integer which represents the highest HTLCKey seen by
// either side within their commitment transaction. Any addition to the pending,
// HTLC lists on either side will increment this height. As a result this value
// should always be monotonically increasing. Any CommitSignature or
// CommitRevocation messages will reference a value for the commitment height
// up to which it covers. HTLCs are only explicitly excluded by sending
// HTLCReject messages referencing a particular HTLCKey.
type CommitHeight uint64

// CreditsAmount are the native currency unit used within the Lightning Network.
// Credits are denominated in sub-satoshi amounts, so micro-satoshis (1/1000).
// This value is purposefully signed in order to allow the expression of negative
// fees.
//
// "In any science-fiction movie, anywhere in the galaxy, currency is referred
// to as 'credits.'"
// 	--Sam Humphries. Ebert, Roger (1999). Ebert's bigger little movie
// 	glossary. Andrews McMeel. p. 172.
//
// https://en.wikipedia.org/wiki/List_of_fictional_currencies
// https://en.wikipedia.org/wiki/Fictional_currency#Trends_in_the_use_of_fictional_currencies
// http://tvtropes.org/pmwiki/pmwiki.php/Main/WeWillSpendCreditsInTheFuture
// US Display format: 1 BTC = 100,000,000'000 XCB
// Or in BTC = 1.00000000'000
// Credits (XCB, accountants should use XCB :^)
type CreditsAmount int64

// ToSatoshi converts an amount in Credits to the coresponding amount
// expressed in Satoshis.
//
// NOTE: This function rounds down by default (floor).
func (c CreditsAmount) ToSatoshi() int64 {
	return int64(c / 1000)
}

// writeElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized for the wire protocol. The passed
// io.Writer should be backed by an appropriatly sized byte slice, or be able
// to dynamically expand to accomdate additional data.
//
// TODO(roasbeef): this should eventually draw from a buffer pool for
// serialization.
// TODO(roasbeef): switch to var-ints for all?
func writeElement(w io.Writer, element interface{}) error {
	switch e := element.(type) {
	case uint8:
		var b [1]byte
		b[0] = byte(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case uint16:
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case CancelReason:
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case ErrorCode:
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case CreditsAmount:
		if err := binary.Write(w, binary.BigEndian, int64(e)); err != nil {
			return err
		}
	case uint32:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case uint64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case HTLCKey:
		if err := binary.Write(w, binary.BigEndian, int64(e)); err != nil {
			return err
		}
	case btcutil.Amount:
		if err := binary.Write(w, binary.BigEndian, int64(e)); err != nil {
			return err
		}
	case *btcec.PublicKey:
		var b [33]byte
		serializedPubkey := e.SerializeCompressed()
		copy(b[:], serializedPubkey)
		// TODO(roasbeef): use WriteVarBytes here?
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case []uint64:
		// Enforce a max number of elements in a uint64 slice.
		numItems := len(e)
		if numItems > 65535 {
			return fmt.Errorf("Too many []uint64s")
		}

		// First write out the the number of elements in the slice as a
		// length prefix.
		if err := writeElement(w, uint16(numItems)); err != nil {
			return err
		}

		// After the prefix detailing the number of elements, write out
		// each uint64 in series.
		for i := 0; i < numItems; i++ {
			if err := writeElement(w, e[i]); err != nil {
				return err
			}
		}
	case []*btcec.Signature:
		// Enforce a sane number for the maximum number of signatures.
		numSigs := len(e)
		if numSigs > 127 {
			return fmt.Errorf("Too many signatures!")
		}

		// First write out the the number of elements in the slice as a
		// length prefix.
		if err := writeElement(w, uint8(numSigs)); err != nil {
			return err
		}

		// After the prefix detailing the number of elements, write out
		// each signature in series.
		for i := 0; i < numSigs; i++ {
			if err := writeElement(w, e[i]); err != nil {
				return err
			}
		}
	case *btcec.Signature:
		var b [64]byte
		err := serializeSigToWire(&b, e)
		if err != nil {
			return err
		}
		// Write buffer
		if _, err = w.Write(b[:]); err != nil {
			return err
		}
	case *chainhash.Hash:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}
	case [][32]byte:
		// First write out the number of elements in the slice.
		sliceSize := len(e)
		if err := writeElement(w, uint16(sliceSize)); err != nil {
			return err
		}

		// Then write each out sequentially.
		for _, element := range e {
			if err := writeElement(w, element); err != nil {
				return err
			}
		}
	case [32]byte:
		// TODO(roasbeef): should be factor out to caller logic...
		if _, err := w.Write(e[:]); err != nil {
			return err
		}
	case [33]byte:
		// TODO(roasbeef): should be factor out to caller logic...
		if _, err := w.Write(e[:]); err != nil {
			return err
		}
	case wire.BitcoinNet:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	case [4]byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}
	case []byte:
		// Enforce the maxmium length of all slices used in the wire
		// protocol.
		sliceLength := len(e)
		if sliceLength > MaxSliceLength {
			return fmt.Errorf("Slice length too long!")
		}

		if err := wire.WriteVarBytes(w, 0, e); err != nil {
			return err
		}
	case PkScript:
		// Make sure it's P2PKH or P2SH size or less.
		scriptLength := len(e)
		if scriptLength > 25 {
			return fmt.Errorf("PkScript too long!")
		}

		if err := wire.WriteVarBytes(w, 0, e); err != nil {
			return err
		}
	case string:
		strlen := len(e)
		if strlen > MaxSliceLength {
			return fmt.Errorf("String too long!")
		}

		if err := wire.WriteVarString(w, 0, e); err != nil {
			return err
		}
	case []*wire.TxIn:
		// Write the size (1-byte)
		if len(e) > 127 {
			return fmt.Errorf("Too many txins")
		}

		// Write out the number of txins.
		if err := writeElement(w, uint8(len(e))); err != nil {
			return err
		}

		// Append the actual TxIns (Size: NumOfTxins * 36)
		// During serialization we leave out the sequence number to
		// eliminate any funny business.
		for _, in := range e {
			if err := writeElement(w, in); err != nil {
				return err
			}
		}
	case *wire.TxIn:
		// First write out the previous txid.
		var h [32]byte
		copy(h[:], e.PreviousOutPoint.Hash[:])
		if _, err := w.Write(h[:]); err != nil {
			return err
		}

		// Then the exact index of the previous out point.
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], e.PreviousOutPoint.Index)
		if _, err := w.Write(idx[:]); err != nil {
			return err
		}
	case *wire.OutPoint:
		// TODO(roasbeef): consolidate with above
		// First write out the previous txid.
		var h [32]byte
		copy(h[:], e.Hash[:])
		if _, err := w.Write(h[:]); err != nil {
			return err
		}

		// Then the exact index of this output.
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], e.Index)
		if _, err := w.Write(idx[:]); err != nil {
			return err
		}
		// TODO(roasbeef): *MsgTx
	case int64, float64:
		err := binary.Write(w, binary.BigEndian, e)
		if err != nil {
			return err
		}
	case ChannelID:
		// Check that field fit in 3 bytes and write the blockHeight
		if e.BlockHeight > ((1 << 24) - 1) {
			return errors.New("block height should fit in 3 bytes")
		}

		var blockHeight [4]byte
		binary.BigEndian.PutUint32(blockHeight[:], e.BlockHeight)

		if _, err := w.Write(blockHeight[1:]); err != nil {
			return err
		}

		// Check that field fit in 3 bytes and write the txIndex
		if e.TxIndex > ((1 << 24) - 1) {
			return errors.New("tx index should fit in 3 bytes")
		}

		var txIndex [4]byte
		binary.BigEndian.PutUint32(txIndex[:], e.TxIndex)
		if _, err := w.Write(txIndex[1:]); err != nil {
			return err
		}

		// Write the txPosition
		var txPosition [2]byte
		binary.BigEndian.PutUint16(txPosition[:], e.TxPosition)
		if _, err := w.Write(txPosition[:]); err != nil {
			return err
		}

	case *net.TCPAddr:
		var ip [16]byte
		copy(ip[:], e.IP.To16())
		if _, err := w.Write(ip[:]); err != nil {
			return err
		}

		var port [4]byte
		binary.BigEndian.PutUint32(port[:], uint32(e.Port))
		if _, err := w.Write(port[:]); err != nil {
			return err
		}
	case RGB:
		err := writeElements(w,
			e.red,
			e.green,
			e.blue,
		)
		if err != nil {
			return err
		}
	case Alias:
		if err := writeElements(w, ([32]byte)(e.data)); err != nil {
			return err
		}

	default:
		return fmt.Errorf("Unknown type in writeElement: %T", e)
	}

	return nil
}

// writeElements is writes each element in the elements slice to the passed
// io.Writer using writeElement.
func writeElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		err := writeElement(w, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// readElement is a one-stop utility function to deserialize any datastructure
// encoded using the serialization format of lnwire.
func readElement(r io.Reader, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *uint8:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = b[0]
	case *CancelReason:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = CancelReason(binary.BigEndian.Uint16(b[:]))
	case *uint16:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint16(b[:])
	case *ErrorCode:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = ErrorCode(binary.BigEndian.Uint16(b[:]))
	case *CreditsAmount:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = CreditsAmount(int64(binary.BigEndian.Uint64(b[:])))
	case *uint32:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint32(b[:])
	case *uint64:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint64(b[:])
	case *HTLCKey:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = HTLCKey(int64(binary.BigEndian.Uint64(b[:])))
	case *btcutil.Amount:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))
	case **chainhash.Hash:
		var b chainhash.Hash
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = &b
	case **btcec.PublicKey:
		var b [btcec.PubKeyBytesLenCompressed]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return err
		}

		pubKey, err := btcec.ParsePubKey(b[:], btcec.S256())
		if err != nil {
			return err
		}
		*e = pubKey
	case *[]uint64:
		var numItems uint16
		if err := readElement(r, &numItems); err != nil {
			return err
		}
		// if numItems > 65535 {
		// 	return fmt.Errorf("Too many items in []uint64")
		// }

		// Read the number of items
		var items []uint64
		for i := uint16(0); i < numItems; i++ {
			var item uint64
			err = readElement(r, &item)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		*e = items
	case *[]*btcec.Signature:
		var numSigs uint8
		err = readElement(r, &numSigs)
		if err != nil {
			return err
		}
		if numSigs > 127 {
			return fmt.Errorf("Too many signatures!")
		}

		// Read that number of signatures
		var sigs []*btcec.Signature
		for i := uint8(0); i < numSigs; i++ {
			sig := new(btcec.Signature)
			err = readElement(r, &sig)
			if err != nil {
				return err
			}
			sigs = append(sigs, sig)
		}
		*e = sigs
		return nil
	case **btcec.Signature:
		var b [64]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		err = deserializeSigFromWire(e, b)
		if err != nil {
			return err
		}
	case *[][32]byte:
		// How many to read
		var sliceSize uint16
		err = readElement(r, &sliceSize)
		if err != nil {
			return err
		}

		data := make([][32]byte, 0, sliceSize)
		// Append the actual
		for i := uint16(0); i < sliceSize; i++ {
			var element [32]byte
			err = readElement(r, &element)
			if err != nil {
				return err
			}
			data = append(data, element)
		}
		*e = data
	case *[32]byte:
		if _, err = io.ReadFull(r, e[:]); err != nil {
			return err
		}
	case *[33]byte:
		if _, err = io.ReadFull(r, e[:]); err != nil {
			return err
		}
	case *wire.BitcoinNet:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = wire.BitcoinNet(binary.BigEndian.Uint32(b[:]))
		return nil
	case *[4]byte:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}
	case *[]byte:
		b, err := wire.ReadVarBytes(r, 0, MaxSliceLength, "byte slice")
		if err != nil {
			return err
		}
		*e = b
	case *PkScript:
		pkScript, err := wire.ReadVarBytes(r, 0, 25, "pkscript")
		if err != nil {
			return err
		}
		*e = pkScript
	case *string:
		str, err := wire.ReadVarString(r, 0)
		if err != nil {
			return err
		}
		*e = str
	case *[]*wire.TxIn:
		// Read the size (1-byte number of txins)
		var numScripts uint8
		if err := readElement(r, &numScripts); err != nil {
			return err
		}
		if numScripts > 127 {
			return fmt.Errorf("Too many txins")
		}

		// Append the actual TxIns
		txins := make([]*wire.TxIn, 0, numScripts)
		for i := uint8(0); i < numScripts; i++ {
			outpoint := new(wire.OutPoint)
			txin := wire.NewTxIn(outpoint, nil, nil)
			if err := readElement(r, &txin); err != nil {
				return err
			}
			txins = append(txins, txin)
		}
		*e = txins
	case **wire.TxIn:
		// Hash
		var h [32]byte
		if _, err = io.ReadFull(r, h[:]); err != nil {
			return err
		}
		hash, err := chainhash.NewHash(h[:])
		if err != nil {
			return err
		}
		(*e).PreviousOutPoint.Hash = *hash

		// Index
		var idxBytes [4]byte
		_, err = io.ReadFull(r, idxBytes[:])
		if err != nil {
			return err
		}
		(*e).PreviousOutPoint.Index = binary.BigEndian.Uint32(idxBytes[:])
		return nil
	case **wire.OutPoint:
		// TODO(roasbeef): consolidate with above
		var h [32]byte
		if _, err = io.ReadFull(r, h[:]); err != nil {
			return err
		}
		hash, err := chainhash.NewHash(h[:])
		if err != nil {
			return err
		}
		// Index
		var idxBytes [4]byte
		_, err = io.ReadFull(r, idxBytes[:])
		if err != nil {
			return err
		}
		index := binary.BigEndian.Uint32(idxBytes[:])

		*e = wire.NewOutPoint(hash, index)
	case *int64, *float64:
		err := binary.Read(r, binary.BigEndian, e)
		if err != nil {
			return err
		}
	case *ChannelID:
		var blockHeight [4]byte
		if _, err = io.ReadFull(r, blockHeight[1:]); err != nil {
			return err
		}

		var txIndex [4]byte
		if _, err = io.ReadFull(r, txIndex[1:]); err != nil {
			return err
		}

		var txPosition [2]byte
		if _, err = io.ReadFull(r, txPosition[:]); err != nil {
			return err
		}

		*e = ChannelID{
			BlockHeight: binary.BigEndian.Uint32(blockHeight[:]),
			TxIndex:     binary.BigEndian.Uint32(txIndex[:]),
			TxPosition:  binary.BigEndian.Uint16(txPosition[:]),
		}

	case **net.TCPAddr:
		var ip [16]byte
		if _, err = io.ReadFull(r, ip[:]); err != nil {
			return err
		}

		var port [4]byte
		if _, err = io.ReadFull(r, port[:]); err != nil {
			return err
		}

		*e = &net.TCPAddr{
			IP:   (net.IP)(ip[:]),
			Port: int(binary.BigEndian.Uint32(port[:])),
		}
	case *RGB:
		err := readElements(r,
			&e.red,
			&e.green,
			&e.blue,
		)
		if err != nil {
			return err
		}
	case *Alias:
		var a [32]byte
		if err := readElements(r, &a); err != nil {
			return err
		}

		*e, err = newAlias(a[:])
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("Unknown type in readElement: %T", e)
	}

	return nil
}

// readElements deserializes a variable number of elements into the passed
// io.Reader, with each element being deserialized according to the readElement
// function.
func readElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		err := readElement(r, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// validatePkScript determines if the passed pkScript is a valid pkScript within
// lnwire. The only pkScript templates that lnwire currently allows are:
// P2SH, P2WSH, P2PKH, and P2WKH.
func isValidPkScript(pkScript PkScript) bool {
	// A nil pkScript is obviously invalid.
	if pkScript == nil {
		return false
	}

	switch len(pkScript) {
	case 25:
		// A valid p2pkh script must be exactly 25 bytes. It must begin
		// with the define prefix, and end with the define suffix.
		p2pkhPrefix := []byte{txscript.OP_DUP, txscript.OP_HASH160}
		p2pkhSuffix := []byte{txscript.OP_EQUALVERIFY, txscript.OP_CHECKSIG,
			txscript.OP_DATA_20}
		if !bytes.Equal(pkScript[0:3], p2pkhPrefix) ||
			!bytes.Equal(pkScript[23:25], p2pkhSuffix) {
			return false
		}
	case 22:
		// P2WKH
		// A valid P2WKH script must be exactly 22 bytes, with the first
		// two op codes being an OP_0 marking a version zero witness
		// program, and the second byte being a 20 byte push data.
		if pkScript[0] != txscript.OP_0 ||
			pkScript[1] != txscript.OP_DATA_20 {
			return false
		}
	case 23:
		// A valid P2SH script must begin with OP_HASH160 PUSHDATA(20),
		// contain 20 bytes, then end with an OP_EQUAL.
		p2shPrefix := []byte{txscript.OP_HASH160, txscript.OP_DATA_20}
		p2shSuffix := []byte{txscript.OP_EQUAL}
		if !bytes.Equal(pkScript[0:2], p2shPrefix) ||
			!bytes.Equal(pkScript[22:23], p2shSuffix) {
			return false
		}
	case 34:
		// A P2WSH script must be exactly 34 bytes, with the first two
		// op codes being an OP_0 marking a version zero witness program,
		// and the second byte being a 32 byte push data.
		if pkScript[0] != txscript.OP_0 ||
			pkScript[1] != txscript.OP_DATA_32 {
			return false
		}
	default:
		return false
	}

	return true
}
