package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"net"
	"unicode/utf8"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
)

// NodeAnnouncement2 message is used to announce the presence of a Lightning
// node and also to signal that the node is accepting incoming connections.
// Each NodeAnnouncement authenticating the advertised information within the
// announcement via a signature using the advertised node pubkey.
type NodeAnnouncement2 struct {
	// Signature is used to prove the ownership of node id.
	Signature Sig

	// Features is the list of protocol features this node supports.
	Features tlv.RecordT[tlv.TlvType0, RawFeatureVector]

	// RGBColor is an optional field used to customize a node's appearance
	// in maps and graphs.
	RGBColor tlv.OptionalRecordT[tlv.TlvType1, RGBColor]

	// BlockHeight allows ordering in the case of multiple announcements.
	BlockHeight tlv.RecordT[tlv.TlvType2, uint32]

	// Alias is used to customize their node's appearance in maps and
	// graphs.
	Alias tlv.OptionalRecordT[tlv.TlvType4, FlexibleNodeAlias]

	// NodeID is a public key which is used as node identification.
	NodeID tlv.RecordT[tlv.TlvType6, [33]byte]

	// IPV4Addresses
	IPV4Addresses tlv.OptionalRecordT[tlv.TlvType3, IPV4Addrs]

	// IPV6Addresses
	IPV6Addresses tlv.OptionalRecordT[tlv.TlvType5, IPV6Addrs]

	// TorV3Addresses
	TorV3Addresses tlv.OptionalRecordT[tlv.TlvType7, TorV3Addrs]

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData ExtraOpaqueData
}

// A compile time check to ensure NodeAnnouncement2 implements the
// lnwire.Message interface.
var _ Message = (*NodeAnnouncement2)(nil)

// Decode deserializes a serialized NodeAnnouncement2 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Decode(r io.Reader, _ uint32) error {
	err := ReadElement(r, &n.Signature)
	if err != nil {
		return err
	}
	n.Signature.ForceSchnorr()

	return n.DecodeTLVRecords(r)
}

func (n *NodeAnnouncement2) DecodeTLVRecords(r io.Reader) error {
	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var (
		rbgColour = tlv.ZeroRecordT[tlv.TlvType1, RGBColor]()
		alias     = tlv.ZeroRecordT[tlv.TlvType4, FlexibleNodeAlias]()
		ipv4      = tlv.ZeroRecordT[tlv.TlvType3, IPV4Addrs]()
		ipv6      = tlv.ZeroRecordT[tlv.TlvType5, IPV6Addrs]()
		torV3     = tlv.ZeroRecordT[tlv.TlvType7, TorV3Addrs]()
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&n.Features, &rbgColour, &n.BlockHeight, &ipv4, &alias,
		&ipv6, &n.NodeID, &torV3,
	)
	if err != nil {
		return err
	}

	if _, ok := typeMap[n.RGBColor.TlvType()]; ok {
		n.RGBColor = tlv.SomeRecordT(rbgColour)
	}
	if _, ok := typeMap[n.Alias.TlvType()]; ok {
		n.Alias = tlv.SomeRecordT(alias)
	}
	if _, ok := typeMap[n.IPV4Addresses.TlvType()]; ok {
		n.IPV4Addresses = tlv.SomeRecordT(ipv4)
	}
	if _, ok := typeMap[n.IPV6Addresses.TlvType()]; ok {
		n.IPV6Addresses = tlv.SomeRecordT(ipv6)
	}
	if _, ok := typeMap[n.TorV3Addresses.TlvType()]; ok {
		n.TorV3Addresses = tlv.SomeRecordT(torV3)
	}

	if len(tlvRecords) != 0 {
		n.ExtraOpaqueData = tlvRecords
	}

	return nil
}

// Encode serializes the target NodeAnnouncement2 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Encode(w *bytes.Buffer, _ uint32) error {
	_, err := w.Write(n.Signature.RawBytes())
	if err != nil {
		return err
	}

	recordProducers := []tlv.RecordProducer{
		&n.Features,
	}

	// Only encode the colour if it is specified.
	n.RGBColor.WhenSome(func(rgb tlv.RecordT[tlv.TlvType1, RGBColor]) {
		recordProducers = append(recordProducers, &rgb)
	})

	recordProducers = append(recordProducers, &n.BlockHeight)

	n.IPV4Addresses.WhenSome(
		func(ipv4 tlv.RecordT[tlv.TlvType3, IPV4Addrs]) {
			recordProducers = append(recordProducers, &ipv4)
		},
	)

	n.Alias.WhenSome(
		func(alias tlv.RecordT[tlv.TlvType4, FlexibleNodeAlias]) {
			recordProducers = append(recordProducers, &alias)
		},
	)

	recordProducers = append(recordProducers, &n.NodeID)

	n.IPV6Addresses.WhenSome(
		func(ipv6 tlv.RecordT[tlv.TlvType5, IPV6Addrs]) {
			recordProducers = append(recordProducers, &ipv6)
		},
	)

	n.TorV3Addresses.WhenSome(
		func(torV3 tlv.RecordT[tlv.TlvType7, TorV3Addrs]) {
			recordProducers = append(recordProducers, &torV3)
		},
	)

	err = EncodeMessageExtraData(&n.ExtraOpaqueData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, n.ExtraOpaqueData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) MsgType() MessageType {
	return MsgNodeAnnouncement2
}

type RGBColor struct {
	color.RGBA
}

func (r *RGBColor) Record() tlv.Record {
	return tlv.MakeStaticRecord(0, r, 3, rgbEncoder, rgbDecoder)
}

func rgbEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*RGBColor); ok {
		buf := bytes.NewBuffer(nil)
		err := WriteColorRGBA(buf, v.RGBA)
		if err != nil {
			return err
		}

		_, err = w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "RGBColor")
}

func rgbDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if v, ok := val.(*RGBColor); ok {
		return ReadElements(r, &v.R, &v.G, &v.B)
	}

	return tlv.NewTypeForDecodingErr(val, "RGBColor", l, 3)
}

// IPV4Addrs is a list of ipv4 addresses that can be encoded as a TLV record.
type IPV4Addrs []*net.TCPAddr

// Record returns a TLV record that can be used to encode/decode IPV4Addrs.
func (i *IPV4Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, i, i.EncodedSize, ipv4AddrsEncoder, ipv4AddrsDecoder,
	)
}

// ipv4AddrEncodedSize is the number of bytes required to encode a single ipv4
// address. Four bytes are used to encode the IP address and two bytes for the
// port number.
const ipv4AddrEncodedSize = 4 + 2

// EncodedSize returns the number of bytes required to encode an IPV4Addrs
// variable.
func (i *IPV4Addrs) EncodedSize() uint64 {
	return uint64(len(*i) * ipv4AddrEncodedSize)
}

func ipv4AddrsEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*IPV4Addrs); ok {
		for _, ip := range *v {
			_, err := w.Write(ip.IP.To4())
			if err != nil {
				return err
			}

			var port [2]byte
			binary.BigEndian.PutUint16(port[:], uint16(ip.Port))

			_, err = w.Write(port[:])

			return err
		}
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV4Addrs")
}

func ipv4AddrsDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*IPV4Addrs); ok {
		if l%(ipv4AddrEncodedSize) != 0 {
			return fmt.Errorf("invalid ipv4 list encoding")
		}

		var (
			numAddrs = int(l / ipv4AddrEncodedSize)
			addrs    = make([]*net.TCPAddr, 0, numAddrs)
			ip       [4]byte
			port     [2]byte
		)
		for len(addrs) < numAddrs {
			_, err := r.Read(ip[:])
			if err != nil {
				return err
			}

			_, err = r.Read(port[:])
			if err != nil {
				return err
			}

			addrs = append(addrs, &net.TCPAddr{
				IP:   ip[:],
				Port: int(binary.BigEndian.Uint16(port[:])),
			})
		}

		*v = addrs

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.IPV4Addrs", l, l)
}

// IPV6Addrs is a list of ipv6 addresses that can be encoded as a TLV record.
type IPV6Addrs []*net.TCPAddr

// ipv6AddrEncodedSize is the number of bytes required to encode a single ipv6
// address. Sixteen bytes are used to encode the IP address and two bytes for
// the port number.
const ipv6AddrEncodedSize = 16 + 2

// EncodedSize returns the number of bytes required to encode an IPV6Addrs
// variable.
func (i *IPV6Addrs) EncodedSize() uint64 {
	return uint64(len(*i) * ipv6AddrEncodedSize)
}

// Record returns a TLV record that can be used to encode/decode IPV6Addrs.
func (i *IPV6Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, i, i.EncodedSize, ipv6AddrsEncoder, ipv6AddrsDecoder,
	)
}

func ipv6AddrsEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*IPV6Addrs); ok {
		for _, ip := range *v {
			_, err := w.Write(ip.IP.To16())
			if err != nil {
				return err
			}

			var port [2]byte
			binary.BigEndian.PutUint16(port[:], uint16(ip.Port))

			_, err = w.Write(port[:])

			return err
		}
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV6Addrs")
}

func ipv6AddrsDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*IPV6Addrs); ok {
		if l%(ipv6AddrEncodedSize) != 0 {
			return fmt.Errorf("invalid ipv6 list encoding")
		}

		var (
			numAddrs = int(l / ipv6AddrEncodedSize)
			addrs    = make([]*net.TCPAddr, 0, numAddrs)
			ip       [16]byte
			port     [2]byte
		)
		for len(addrs) < numAddrs {
			_, err := r.Read(ip[:])
			if err != nil {
				return err
			}

			_, err = r.Read(port[:])
			if err != nil {
				return err
			}

			addrs = append(addrs, &net.TCPAddr{
				IP:   ip[:],
				Port: int(binary.BigEndian.Uint16(port[:])),
			})
		}

		*v = addrs

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.IPV6Addrs", l, l)
}

// TorV3Addrs is a list of tor v3 addresses that can be encoded as a TLV record.
type TorV3Addrs []*tor.OnionAddr

// torV3AddrEncodedSize is the number of bytes required to encode a single tor
// v3 address.
const torV3AddrEncodedSize = tor.V3DecodedLen + 2

// EncodedSize returns the number of bytes required to encode an TorV3Addrs
// variable.
func (i *TorV3Addrs) EncodedSize() uint64 {
	return uint64(len(*i) * torV3AddrEncodedSize)
}

// Record returns a TLV record that can be used to encode/decode TorV3Addrs.
func (i *TorV3Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, i, i.EncodedSize, torV3AddrsEncoder, torV3AddrsDecoder,
	)
}

func torV3AddrsEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*TorV3Addrs); ok {
		for _, addr := range *v {
			encodedHostLen := tor.V3Len - tor.OnionSuffixLen
			host, err := tor.Base32Encoding.DecodeString(
				addr.OnionService[:encodedHostLen],
			)
			if err != nil {
				return err
			}

			if len(host) != tor.V3DecodedLen {
				return fmt.Errorf("expected a tor v3 host "+
					"length of %d, got: %d",
					tor.V2DecodedLen, len(host))
			}

			if _, err = w.Write(host); err != nil {
				return err
			}

			var port [2]byte
			binary.BigEndian.PutUint16(port[:], uint16(addr.Port))

			_, err = w.Write(port[:])

			return err
		}
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.TorV3Addrs")
}

func torV3AddrsDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*TorV3Addrs); ok {
		if l%torV3AddrEncodedSize != 0 {
			return fmt.Errorf("invalid tor v3 list encoding")
		}

		var (
			numAddrs = int(l / torV3AddrEncodedSize)
			addrs    = make([]*tor.OnionAddr, 0, numAddrs)
			ip       [tor.V3DecodedLen]byte
			p        [2]byte
		)
		for len(addrs) < numAddrs {
			_, err := r.Read(ip[:])
			if err != nil {
				return err
			}

			_, err = r.Read(p[:])
			if err != nil {
				return err
			}

			onionService := tor.Base32Encoding.EncodeToString(ip[:])
			onionService += tor.OnionSuffix
			port := int(binary.BigEndian.Uint16(p[:]))

			addrs = append(addrs, &tor.OnionAddr{
				OnionService: onionService,
				Port:         port,
			})
		}

		*v = addrs

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.TorV3Addrs", l, l)
}

// FlexibleNodeAlias is a hex encoded UTF-8 string that may be displayed as an
// alternative to the node's ID. An alias does not need to be unique and may be
// freely chosen by the node operators. An alias may be anywhere from 0 to 32
// byes long.
type FlexibleNodeAlias []byte

// Record returns a TLV record that can be used to encode/decode a
// FlexibleNodeAlias.
func (a *FlexibleNodeAlias) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, a, func() uint64 {
			return uint64(len(*a))
		}, encodeFlexibleAlias, decodeFlexibleAlias,
	)
}

func decodeFlexibleAlias(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*FlexibleNodeAlias); ok {
		if l > 32 {
			return fmt.Errorf("node alias (len=%d) violates "+
				"maximum length of 32", l)
		}

		b := make([]byte, l)

		if _, err := r.Read(b); err != nil {
			return err
		}

		*v = b

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.FlexibleNodeAlias", l, l)
}

func encodeFlexibleAlias(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*FlexibleNodeAlias); ok {
		if len(*v) > 32 {
			return fmt.Errorf("a node alias must be no longer " +
				"than 32 bytes")
		}

		// Validate the alias.
		if !utf8.ValidString(string(*v)) {
			return fmt.Errorf("node alias has non-utf8 characters")
		}

		var buf bytes.Buffer
		if err := WriteBytes(&buf, *v); err != nil {
			return err
		}

		_, err := w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.FlexibleNodeAlias")
}
