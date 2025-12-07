package lnwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"io"
	"math"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tor"
)

const (
	// MaxSliceLength is the maximum allowed length for any opaque byte
	// slices in the wire protocol.
	MaxSliceLength = 65535

	// MaxMsgBody is the largest payload any message is allowed to provide.
	// This is two less than the MaxSliceLength as each message has a 2
	// byte type that precedes the message body.
	MaxMsgBody = 65533
)

const (
	// tcp4AddrLen is the length of an IPv4 address
	// (4 bytes IP + 2 bytes port).
	tcp4AddrLen = 6

	// tcp6AddrLen is the length of an IPv6 address
	// (16 bytes IP + 2 bytes port).
	tcp6AddrLen = 18

	// v2OnionAddrLen is the length of a version 2 Tor onion service
	// address.
	v2OnionAddrLen = 12

	// v3OnionAddrLen is the length of a version 3 Tor onion service address
	// (35 bytes decoded onion + 2 bytes port).
	v3OnionAddrLen = 37

	// dnsAddrOverhead is the fixed overhead for a DNS address: 1 byte for
	// the hostname length and 2 bytes for the port.
	dnsAddrOverhead = 3
)

// PkScript is simple type definition which represents a raw serialized public
// key script.
type PkScript []byte

// addressType specifies the network protocol and version that should be used
// when connecting to a node at a particular address.
type addressType uint8

const (
	// noAddr denotes a blank address. An address of this type indicates
	// that a node doesn't have any advertised addresses.
	noAddr addressType = 0

	// tcp4Addr denotes an IPv4 TCP address.
	tcp4Addr addressType = 1

	// tcp6Addr denotes an IPv6 TCP address.
	tcp6Addr addressType = 2

	// v2OnionAddr denotes a version 2 Tor onion service address.
	v2OnionAddr addressType = 3

	// v3OnionAddr denotes a version 3 Tor (prop224) onion service address.
	v3OnionAddr addressType = 4

	// dnsAddr denotes a DNS address.
	dnsAddr addressType = 5
)

// WriteElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized for the wire protocol.
//
// TODO(yy): rm this method once we finish dereferencing it from other
// packages.
func WriteElement(w *bytes.Buffer, element interface{}) error {
	switch e := element.(type) {
	case NodeAlias:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case QueryEncoding:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint8:
		var b [1]byte
		b[0] = e
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case FundingFlag:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint16:
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ChanUpdateMsgFlags:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ChanUpdateChanFlags:
		var b [1]byte
		b[0] = uint8(e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case MilliSatoshi:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case btcutil.Amount:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(e))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint32:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case uint64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], e)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case *btcec.PublicKey:
		if e == nil {
			return fmt.Errorf("cannot write nil pubkey")
		}

		var b [33]byte
		serializedPubkey := e.SerializeCompressed()
		copy(b[:], serializedPubkey)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case []Sig:
		var b [2]byte
		numSigs := uint16(len(e))
		binary.BigEndian.PutUint16(b[:], numSigs)
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

		for _, sig := range e {
			if err := WriteElement(w, sig); err != nil {
				return err
			}
		}

	case Sig:
		// Write buffer
		if _, err := w.Write(e.bytes[:]); err != nil {
			return err
		}

	case PartialSig:
		if err := e.Encode(w); err != nil {
			return err
		}

	case PingPayload:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case PongPayload:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case WarningData:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case ErrorData:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case OpaqueReason:
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(e)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}

		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case [33]byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case []byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case PkScript:
		// The largest script we'll accept is a p2wsh which is exactly
		// 34 bytes long.
		scriptLength := len(e)
		if scriptLength > 34 {
			return fmt.Errorf("'PkScript' too long")
		}

		if err := wire.WriteVarBytes(w, 0, e); err != nil {
			return err
		}

	case *RawFeatureVector:
		if e == nil {
			return fmt.Errorf("cannot write nil feature vector")
		}

		if err := e.Encode(w); err != nil {
			return err
		}

	case wire.OutPoint:
		var h [32]byte
		copy(h[:], e.Hash[:])
		if _, err := w.Write(h[:]); err != nil {
			return err
		}

		if e.Index > math.MaxUint16 {
			return fmt.Errorf("index for outpoint (%v) is "+
				"greater than max index of %v", e.Index,
				math.MaxUint16)
		}

		var idx [2]byte
		binary.BigEndian.PutUint16(idx[:], uint16(e.Index))
		if _, err := w.Write(idx[:]); err != nil {
			return err
		}

	case ChannelID:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case FailCode:
		if err := WriteElement(w, uint16(e)); err != nil {
			return err
		}

	case ShortChannelID:
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
		if err := WriteTCPAddr(w, e); err != nil {
			return err
		}

	case *tor.OnionAddr:
		if err := WriteOnionAddr(w, e); err != nil {
			return err
		}

	case *DNSAddress:
		if err := WriteDNSAddress(w, e); err != nil {
			return err
		}

	case *OpaqueAddrs:
		if err := WriteOpaqueAddrs(w, e); err != nil {
			return err
		}

	case []net.Addr:
		// First, we'll encode all the addresses into an intermediate
		// buffer. We need to do this in order to compute the total
		// length of the addresses.
		var addrBuf bytes.Buffer
		for _, address := range e {
			if err := WriteElement(&addrBuf, address); err != nil {
				return err
			}
		}

		// With the addresses fully encoded, we can now write out the
		// number of bytes needed to encode them.
		addrLen := addrBuf.Len()
		if err := WriteElement(w, uint16(addrLen)); err != nil {
			return err
		}

		// Finally, we'll write out the raw addresses themselves, but
		// only if we have any bytes to write.
		if addrLen > 0 {
			if _, err := w.Write(addrBuf.Bytes()); err != nil {
				return err
			}
		}

	case color.RGBA:
		if err := WriteElements(w, e.R, e.G, e.B); err != nil {
			return err
		}

	case DeliveryAddress:
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(e)))
		if _, err := w.Write(length[:]); err != nil {
			return err
		}
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case bool:
		var b [1]byte
		if e {
			b[0] = 1
		}
		if _, err := w.Write(b[:]); err != nil {
			return err
		}

	case ExtraOpaqueData:
		return e.Encode(w)

	default:
		return fmt.Errorf("unknown type in WriteElement: %T", e)
	}

	return nil
}

// WriteElements is writes each element in the elements slice to the passed
// buffer using WriteElement.
//
// TODO(yy): rm this method once we finish dereferencing it from other
// packages.
func WriteElements(buf *bytes.Buffer, elements ...interface{}) error {
	for _, element := range elements {
		err := WriteElement(buf, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadElement is a one-stop utility function to deserialize any datastructure
// encoded using the serialization format of lnwire.
func ReadElement(r io.Reader, element interface{}) error {
	var err error
	switch e := element.(type) {
	case *bool:
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}

		if b[0] == 1 {
			*e = true
		}

	case *NodeAlias:
		var a [32]byte
		if _, err := io.ReadFull(r, a[:]); err != nil {
			return err
		}

		alias, err := NewNodeAlias(string(a[:]))
		if err != nil {
			return err
		}
		*e = alias

	case *QueryEncoding:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = QueryEncoding(b[0])

	case *uint8:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = b[0]

	case *FundingFlag:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = FundingFlag(b[0])

	case *uint16:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = binary.BigEndian.Uint16(b[:])

	case *ChanUpdateMsgFlags:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = ChanUpdateMsgFlags(b[0])

	case *ChanUpdateChanFlags:
		var b [1]uint8
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		*e = ChanUpdateChanFlags(b[0])

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

	case *MilliSatoshi:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = MilliSatoshi(int64(binary.BigEndian.Uint64(b[:])))

	case *btcutil.Amount:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		*e = btcutil.Amount(int64(binary.BigEndian.Uint64(b[:])))

	case **btcec.PublicKey:
		var b [btcec.PubKeyBytesLenCompressed]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return err
		}

		pubKey, err := btcec.ParsePubKey(b[:])
		if err != nil {
			return err
		}
		*e = pubKey

	case *RawFeatureVector:
		f := NewRawFeatureVector()
		err = f.Decode(r)
		if err != nil {
			return err
		}
		*e = *f

	case **RawFeatureVector:
		f := NewRawFeatureVector()
		err = f.Decode(r)
		if err != nil {
			return err
		}
		*e = f

	case *[]Sig:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		numSigs := binary.BigEndian.Uint16(l[:])

		var sigs []Sig
		if numSigs > 0 {
			sigs = make([]Sig, numSigs)
			for i := 0; i < int(numSigs); i++ {
				if err := ReadElement(r, &sigs[i]); err != nil {
					return err
				}
			}
		}
		*e = sigs

	case *Sig:
		if _, err := io.ReadFull(r, e.bytes[:]); err != nil {
			return err
		}

	case *OpaqueReason:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		reasonLen := binary.BigEndian.Uint16(l[:])

		*e = OpaqueReason(make([]byte, reasonLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *WarningData:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		errorLen := binary.BigEndian.Uint16(l[:])

		*e = WarningData(make([]byte, errorLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *ErrorData:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		errorLen := binary.BigEndian.Uint16(l[:])

		*e = ErrorData(make([]byte, errorLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *PingPayload:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		pingLen := binary.BigEndian.Uint16(l[:])

		*e = PingPayload(make([]byte, pingLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *PongPayload:
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return err
		}
		pongLen := binary.BigEndian.Uint16(l[:])

		*e = PongPayload(make([]byte, pongLen))
		if _, err := io.ReadFull(r, *e); err != nil {
			return err
		}

	case *[33]byte:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case []byte:
		if _, err := io.ReadFull(r, e); err != nil {
			return err
		}

	case *PkScript:
		pkScript, err := wire.ReadVarBytes(r, 0, 34, "pkscript")
		if err != nil {
			return err
		}
		*e = pkScript

	case *wire.OutPoint:
		var h [32]byte
		if _, err = io.ReadFull(r, h[:]); err != nil {
			return err
		}
		hash, err := chainhash.NewHash(h[:])
		if err != nil {
			return err
		}

		var idxBytes [2]byte
		_, err = io.ReadFull(r, idxBytes[:])
		if err != nil {
			return err
		}
		index := binary.BigEndian.Uint16(idxBytes[:])

		*e = wire.OutPoint{
			Hash:  *hash,
			Index: uint32(index),
		}

	case *FailCode:
		if err := ReadElement(r, (*uint16)(e)); err != nil {
			return err
		}

	case *ChannelID:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *ShortChannelID:
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

		*e = ShortChannelID{
			BlockHeight: binary.BigEndian.Uint32(blockHeight[:]),
			TxIndex:     binary.BigEndian.Uint32(txIndex[:]),
			TxPosition:  binary.BigEndian.Uint16(txPosition[:]),
		}

	case *[]net.Addr:
		// First, we'll read the number of total bytes that have been
		// used to encode the set of addresses.
		var numAddrsBytes [2]byte
		if _, err = io.ReadFull(r, numAddrsBytes[:]); err != nil {
			return err
		}
		addrsLen := binary.BigEndian.Uint16(numAddrsBytes[:])

		// With the number of addresses, read, we'll now pull in the
		// buffer of the encoded addresses into memory.
		addrs := make([]byte, addrsLen)
		if _, err := io.ReadFull(r, addrs[:]); err != nil {
			return err
		}
		addrBuf := bytes.NewReader(addrs)

		// Finally, we'll parse the remaining address payload in
		// series, using the first byte to denote how to decode the
		// address itself.
		var (
			addresses     []net.Addr
			addrBytesRead uint16
		)

		for addrBytesRead < addrsLen {
			bytesRead, address, err := ReadAddress(
				addrBuf, addrsLen-addrBytesRead,
			)
			if err != nil {
				return fmt.Errorf("unable to read address: %w",
					err)
			}
			addrBytesRead += bytesRead

			// If we encounter a noAddr descriptor, then we'll move
			// on to the next address.
			if address == nil {
				continue
			}

			addresses = append(addresses, address)
		}

		*e = addresses

	case *color.RGBA:
		err := ReadElements(r,
			&e.R,
			&e.G,
			&e.B,
		)
		if err != nil {
			return err
		}

	case *DeliveryAddress:
		var addrLen [2]byte
		if _, err = io.ReadFull(r, addrLen[:]); err != nil {
			return err
		}
		length := binary.BigEndian.Uint16(addrLen[:])

		var addrBytes [deliveryAddressMaxSize]byte

		if length > deliveryAddressMaxSize {
			return fmt.Errorf(
				"cannot read %d bytes into addrBytes", length,
			)
		}
		if _, err = io.ReadFull(r, addrBytes[:length]); err != nil {
			return err
		}
		*e = addrBytes[:length]

	case *PartialSig:
		var sig PartialSig
		if err = sig.Decode(r); err != nil {
			return err
		}
		*e = sig

	case *ExtraOpaqueData:
		return e.Decode(r)

	default:
		return fmt.Errorf("unknown type in ReadElement: %T", e)
	}

	return nil
}

// ReadElements deserializes a variable number of elements into the passed
// io.Reader, with each element being deserialized according to the ReadElement
// function.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		err := ReadElement(r, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadAddress attempts to read a single address descriptor (as defined in
// Bolt 7) from the passed io.Reader. The total length of the address section
// (in bytes) must be provided so that we can ensure we don't read beyond the
// end of the address section. The number of bytes read from the reader and the
// parsed net.Addr are returned.
//
// NOTE: it is possible for the number of bytes read to be 1 even if a nil
// address is returned.
func ReadAddress(addrBuf io.Reader, addrsLen uint16) (uint16, net.Addr, error) {
	var descriptor [1]byte
	if _, err := io.ReadFull(addrBuf, descriptor[:]); err != nil {
		return 0, nil, err
	}

	addrBytesRead := uint16(1)

	var address net.Addr
	switch aType := addressType(descriptor[0]); aType {
	case noAddr:
		return addrBytesRead, nil, nil

	case tcp4Addr:
		var ip [4]byte
		if _, err := io.ReadFull(addrBuf, ip[:]); err != nil {
			return 0, nil, err
		}

		var port [2]byte
		if _, err := io.ReadFull(addrBuf, port[:]); err != nil {
			return 0, nil, err
		}

		address = &net.TCPAddr{
			IP:   net.IP(ip[:]),
			Port: int(binary.BigEndian.Uint16(port[:])),
		}
		addrBytesRead += tcp4AddrLen

	case tcp6Addr:
		var ip [16]byte
		if _, err := io.ReadFull(addrBuf, ip[:]); err != nil {
			return 0, nil, err
		}

		var port [2]byte
		if _, err := io.ReadFull(addrBuf, port[:]); err != nil {
			return 0, nil, err
		}

		address = &net.TCPAddr{
			IP:   net.IP(ip[:]),
			Port: int(binary.BigEndian.Uint16(port[:])),
		}
		addrBytesRead += tcp6AddrLen

	case v2OnionAddr:
		var h [tor.V2DecodedLen]byte
		if _, err := io.ReadFull(addrBuf, h[:]); err != nil {
			return 0, nil, err
		}

		var p [2]byte
		if _, err := io.ReadFull(addrBuf, p[:]); err != nil {
			return 0, nil, err
		}

		onionService := tor.Base32Encoding.EncodeToString(h[:])
		onionService += tor.OnionSuffix
		port := int(binary.BigEndian.Uint16(p[:]))

		address = &tor.OnionAddr{
			OnionService: onionService,
			Port:         port,
		}
		addrBytesRead += v2OnionAddrLen

	case v3OnionAddr:
		var h [tor.V3DecodedLen]byte
		if _, err := io.ReadFull(addrBuf, h[:]); err != nil {
			return 0, nil, err
		}

		var p [2]byte
		if _, err := io.ReadFull(addrBuf, p[:]); err != nil {
			return 0, nil, err
		}

		onionService := tor.Base32Encoding.EncodeToString(h[:])
		onionService += tor.OnionSuffix
		port := int(binary.BigEndian.Uint16(p[:]))

		address = &tor.OnionAddr{
			OnionService: onionService,
			Port:         port,
		}
		addrBytesRead += v3OnionAddrLen

	case dnsAddr:
		var hostnameLen [1]byte
		if _, err := io.ReadFull(addrBuf, hostnameLen[:]); err != nil {
			return 0, nil, err
		}

		hostname := make([]byte, hostnameLen[0])
		if _, err := io.ReadFull(addrBuf, hostname); err != nil {
			return 0, nil, err
		}

		var port [2]byte
		if _, err := io.ReadFull(addrBuf, port[:]); err != nil {
			return 0, nil, err
		}

		address = &DNSAddress{
			Hostname: string(hostname),
			Port:     binary.BigEndian.Uint16(port[:]),
		}
		addrBytesRead += dnsAddrOverhead + uint16(len(hostname))

	default:
		// If we don't understand this address type, we just store it
		// along with the remaining address bytes as type OpaqueAddrs.
		// We need to hold onto the bytes so that we can still write
		// them back to the wire when we propagate this message.
		payloadLen := 1 + addrsLen - addrBytesRead
		payload := make([]byte, payloadLen)

		// First write a byte for the address type that we already read.
		payload[0] = byte(aType)

		// Now append the rest of the address bytes.
		if _, err := io.ReadFull(addrBuf, payload[1:]); err != nil {
			return 0, nil, err
		}

		address = &OpaqueAddrs{
			Payload: payload,
		}
		addrBytesRead = addrsLen
	}

	return addrBytesRead, address, nil
}
