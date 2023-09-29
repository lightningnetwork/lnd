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

const (
	// NodeAnn2FeaturesType is the tlv number associated with the features
	// vector TLV record in the node_announcement_2 message.
	NodeAnn2FeaturesType = tlv.Type(0)

	// NodeAnn2RGBColorType is the tlv number associated with the color TLV
	// record in the node_announcement_2 message.
	NodeAnn2RGBColorType = tlv.Type(1)

	// NodeAnn2BlockHeightType is the tlv number associated with the block
	// height TLV record in the node_announcement_2 message.
	NodeAnn2BlockHeightType = tlv.Type(2)

	// NodeAnn2AliasType is the tlv number associated with the alias vector
	// TLV record in the node_announcement_2 message.
	NodeAnn2AliasType = tlv.Type(3)

	// NodeAnn2NodeIDType is the tlv number associated with the node ID TLV
	// record in the node_announcement_2 message.
	NodeAnn2NodeIDType = tlv.Type(4)

	// NodeAnn2IPV4AddrsType is the tlv number associated with the ipv4
	// addresses TLV record in the node_announcement_2 message.
	NodeAnn2IPV4AddrsType = tlv.Type(5)

	// NodeAnn2IPV6AddrsType is the tlv number associated with the ipv6
	// addresses TLV record in the node_announcement_2 message.
	NodeAnn2IPV6AddrsType = tlv.Type(7)

	// NodeAnn2TorV3AddrsType is the tlv number associated with the tor V3
	// addresses TLV record in the node_announcement_2 message.
	NodeAnn2TorV3AddrsType = tlv.Type(9)
)

// NodeAnnouncement2 message is used to announce the presence of a Lightning
// node and also to signal that the node is accepting incoming connections.
// Each NodeAnnouncement authenticating the advertised information within the
// announcement via a signature using the advertised node pubkey.
type NodeAnnouncement2 struct {
	// Signature is used to prove the ownership of node id.
	Signature Sig

	// Features is the list of protocol features this node supports.
	Features RawFeatureVector

	// RGBColor is an optional field used to customize a node's appearance
	// in maps and graphs.
	RGBColor *color.RGBA

	// BlockHeight allows ordering in the case of multiple announcements.
	BlockHeight uint32

	// Alias is used to customize their node's appearance in maps and
	// graphs.
	Alias []byte

	// NodeID is a public key which is used as node identification.
	NodeID [33]byte

	// Address are addresses on which the node is accepting incoming
	// connections.
	Addresses []net.Addr

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

// Decode deserializes a serialized AnnounceSignatures stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Decode(r io.Reader, _ uint32) error {
	err := ReadElement(r, &n.Signature)
	if err != nil {
		return err
	}
	n.Signature.ForceSchnorr()

	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	featuresRecordProducer := NewRawFeatureVectorRecordProducer(
		NodeAnn2FeaturesType,
	)

	var (
		rbgColour color.RGBA
		alias     []byte
		ipv4      IPV4Addrs
		ipv6      IPV6Addrs
		torV3     TorV3Addrs
	)
	records := []tlv.Record{
		featuresRecordProducer.Record(),
		tlv.MakeStaticRecord(
			NodeAnn2RGBColorType, &rbgColour, 3, rgbEncoder,
			rgbDecoder,
		),
		tlv.MakePrimitiveRecord(
			NodeAnn2BlockHeightType, &n.BlockHeight,
		),
		tlv.MakePrimitiveRecord(NodeAnn2AliasType, &alias),
		tlv.MakePrimitiveRecord(NodeAnn2NodeIDType, &n.NodeID),
		tlv.MakeDynamicRecord(
			NodeAnn2IPV4AddrsType, &ipv4, ipv4.EncodedSize,
			ipv4AddrsEncoder, ipv4AddrsDecoder,
		),
		tlv.MakeDynamicRecord(
			NodeAnn2IPV6AddrsType, &ipv6, ipv6.EncodedSize,
			ipv6AddrsEncoder, ipv6AddrsDecoder,
		),
		tlv.MakeDynamicRecord(
			NodeAnn2TorV3AddrsType, &torV3, torV3.EncodedSize,
			torV3AddrsEncoder, torV3AddrsDecoder,
		),
	}

	typeMap, err := tlvRecords.ExtractRecords(records...)
	if err != nil {
		return err
	}

	if _, ok := typeMap[NodeAnn2FeaturesType]; ok {
		n.Features = featuresRecordProducer.RawFeatureVector
	}

	if _, ok := typeMap[NodeAnn2RGBColorType]; ok {
		n.RGBColor = &rbgColour
	}

	if _, ok := typeMap[NodeAnn2AliasType]; ok {
		// TODO(elle): do this before we allocate the bytes for it
		//  somehow?
		if len(alias) > 32 {
			return fmt.Errorf("alias too large: max is %v, got %v",
				32, len(alias))
		}

		// Validate the alias.
		if !utf8.ValidString(string(alias)) {
			return fmt.Errorf("node alias has non-utf8 characters")
		}

		n.Alias = alias
	}

	if _, ok := typeMap[NodeAnn2IPV4AddrsType]; ok {
		for _, addr := range ipv4 {
			n.Addresses = append(n.Addresses, net.Addr(addr))
		}
	}

	if _, ok := typeMap[NodeAnn2IPV6AddrsType]; ok {
		for _, addr := range ipv6 {
			n.Addresses = append(n.Addresses, net.Addr(addr))
		}
	}

	if _, ok := typeMap[NodeAnn2TorV3AddrsType]; ok {
		for _, addr := range torV3 {
			n.Addresses = append(n.Addresses, net.Addr(addr))
		}
	}

	if len(tlvRecords) != 0 {
		n.ExtraOpaqueData = tlvRecords
	}

	return nil
}

// Encode serializes the target AnnounceSignatures into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Encode(w *bytes.Buffer, _ uint32) error {
	_, err := w.Write(n.Signature.RawBytes())
	if err != nil {
		return err
	}

	featuresRecordProducer := &RawFeatureVectorRecordProducer{
		RawFeatureVector: n.Features,
		Type:             NodeAnn2FeaturesType,
	}

	records := []tlv.Record{
		featuresRecordProducer.Record(),
	}

	// Only encode the colour if it is specified.
	if n.RGBColor != nil {
		records = append(records, tlv.MakeStaticRecord(
			NodeAnn2RGBColorType, n.RGBColor, 3, rgbEncoder,
			rgbDecoder,
		))
	}

	records = append(records, tlv.MakePrimitiveRecord(
		NodeAnn2BlockHeightType, &n.BlockHeight,
	))

	if len(n.Alias) != 0 {
		records = append(records,
			tlv.MakePrimitiveRecord(NodeAnn2AliasType, &n.Alias),
		)
	}

	records = append(
		records, tlv.MakePrimitiveRecord(NodeAnn2NodeIDType, &n.NodeID),
	)

	// Iterate over the addresses and collect the various types.
	var (
		ipv4  IPV4Addrs
		ipv6  IPV6Addrs
		torv3 TorV3Addrs
	)
	for _, addr := range n.Addresses {
		switch a := addr.(type) {
		case *net.TCPAddr:
			if a.IP.To4() != nil {
				ipv4 = append(ipv4, a)
			} else {
				ipv6 = append(ipv6, a)
			}

		case *tor.OnionAddr:
			torv3 = append(torv3, a)
		}
	}

	if len(ipv4) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			NodeAnn2IPV4AddrsType, &ipv4, ipv4.EncodedSize,
			ipv4AddrsEncoder, ipv4AddrsDecoder,
		))
	}

	if len(ipv6) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			NodeAnn2IPV6AddrsType, &ipv6, ipv6.EncodedSize,
			ipv6AddrsEncoder, ipv6AddrsDecoder,
		))
	}

	if len(torv3) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			NodeAnn2TorV3AddrsType, &torv3, torv3.EncodedSize,
			torV3AddrsEncoder, torV3AddrsDecoder,
		))
	}

	err = EncodeMessageExtraDataFromRecords(&n.ExtraOpaqueData, records...)
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

func rgbEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*color.RGBA); ok {
		buf := bytes.NewBuffer(nil)
		err := WriteColorRGBA(buf, *v)
		if err != nil {
			return err
		}

		_, err = w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "color.RGBA")
}

func rgbDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if v, ok := val.(*color.RGBA); ok {
		return ReadElements(r, &v.R, &v.G, &v.B)
	}

	return tlv.NewTypeForDecodingErr(val, "color.RGBA", l, 3)
}

// IPV4Addrs is a list of ipv4 addresses that can be encoded as a TLV record.
type IPV4Addrs []*net.TCPAddr

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

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV4Addrs")
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

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV6Addrs")
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

	return tlv.NewTypeForEncodingErr(val, "lnwire.TorV3Addrs")
}
