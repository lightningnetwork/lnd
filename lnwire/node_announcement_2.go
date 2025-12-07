package lnwire

import (
	"bytes"
	"encoding/binary"
	"errors"
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
// Each NodeAnnouncement2 authenticating the advertised information within the
// announcement via a signature using the advertised node pubkey.
type NodeAnnouncement2 struct {
	// Features is the feature vector that encodes the features supported
	// by the target node.
	Features tlv.RecordT[tlv.TlvType0, RawFeatureVector]

	// Color is an optional field used to customize a node's appearance in
	// maps and graphs.
	Color tlv.OptionalRecordT[tlv.TlvType1, Color]

	// BlockHeight allows ordering in the case of multiple announcements. We
	// should ignore the message if block height is not greater than the
	// last-received. The block height must always be greater or equal to
	// the block height that the channel funding transaction was confirmed
	// in.
	BlockHeight tlv.RecordT[tlv.TlvType2, uint32]

	// Alias is used to customize their node's appearance in maps and
	// graphs.
	Alias tlv.OptionalRecordT[tlv.TlvType3, NodeAlias2]

	// NodeID is the public key of the node creating the announcement.
	NodeID tlv.RecordT[tlv.TlvType4, [33]byte]

	// IPV4Addrs is an optional list of ipv4 addresses that the node is
	// reachable at.
	IPV4Addrs tlv.OptionalRecordT[tlv.TlvType5, IPV4Addrs]

	// IPV6Addrs is an optional list of ipv6 addresses that the node is
	// reachable at.
	IPV6Addrs tlv.OptionalRecordT[tlv.TlvType7, IPV6Addrs]

	// TorV3Addrs is an optional list of tor v3 addresses that the node is
	// reachable at.
	TorV3Addrs tlv.OptionalRecordT[tlv.TlvType9, TorV3Addrs]

	// DNSHostName is an optional DNS hostname that the node is reachable
	// at.
	DNSHostName tlv.OptionalRecordT[tlv.TlvType11, DNSAddress]

	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature tlv.RecordT[tlv.TlvType160, Sig]

	// Any extra fields in the signed range that we do not yet know about,
	// but we need to keep them for signature validation and to produce a
	// valid message.
	ExtraSignedFields
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (n *NodeAnnouncement2) SerializedSize() (uint32, error) {
	return MessageSerializedSize(n)
}

// AllRecords returns all the TLV records for the message. This will include all
// the records we know about along with any that we don't know about but that
// fall in the signed TLV range.
//
// NOTE: this is part of the PureTLVMessage interface.
func (n *NodeAnnouncement2) AllRecords() []tlv.Record {
	recordProducers := []tlv.RecordProducer{
		&n.Features,
		&n.BlockHeight,
		&n.NodeID,
		&n.Signature,
	}

	n.Color.WhenSome(func(r tlv.RecordT[tlv.TlvType1, Color]) {
		recordProducers = append(recordProducers, &r)
	})

	n.Alias.WhenSome(func(a tlv.RecordT[tlv.TlvType3, NodeAlias2]) {
		recordProducers = append(recordProducers, &a)
	})

	n.IPV4Addrs.WhenSome(func(r tlv.RecordT[tlv.TlvType5, IPV4Addrs]) {
		recordProducers = append(recordProducers, &r)
	})

	n.IPV6Addrs.WhenSome(func(r tlv.RecordT[tlv.TlvType7, IPV6Addrs]) {
		recordProducers = append(recordProducers, &r)
	})

	n.TorV3Addrs.WhenSome(func(r tlv.RecordT[tlv.TlvType9, TorV3Addrs]) {
		recordProducers = append(recordProducers, &r)
	})

	n.DNSHostName.WhenSome(func(r tlv.RecordT[tlv.TlvType11, DNSAddress]) {
		recordProducers = append(recordProducers, &r)
	})

	recordProducers = append(recordProducers, RecordsAsProducers(
		tlv.MapToRecords(n.ExtraSignedFields),
	)...)

	return ProduceRecordsSorted(recordProducers...)
}

// Decode deserializes a serialized NodeAnnouncement2 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Decode(r io.Reader, _ uint32) error {
	var (
		color = tlv.ZeroRecordT[tlv.TlvType1, Color]()
		alias = tlv.ZeroRecordT[tlv.TlvType3, NodeAlias2]()
		ipv4  = tlv.ZeroRecordT[tlv.TlvType5, IPV4Addrs]()
		ipv6  = tlv.ZeroRecordT[tlv.TlvType7, IPV6Addrs]()
		torV3 = tlv.ZeroRecordT[tlv.TlvType9, TorV3Addrs]()
		dns   = tlv.ZeroRecordT[tlv.TlvType11, DNSAddress]()
	)

	stream, err := tlv.NewStream(ProduceRecordsSorted(
		&n.Features,
		&n.BlockHeight,
		&n.NodeID,
		&n.Signature,
		&alias,
		&color,
		&ipv4,
		&ipv6,
		&torV3,
		&dns,
	)...)
	if err != nil {
		return err
	}
	n.Signature.Val.ForceSchnorr()

	typeMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return err
	}

	if _, ok := typeMap[n.Alias.TlvType()]; ok {
		n.Alias = tlv.SomeRecordT(alias)
	}

	if _, ok := typeMap[n.Color.TlvType()]; ok {
		n.Color = tlv.SomeRecordT(color)
	}

	if _, ok := typeMap[n.IPV4Addrs.TlvType()]; ok {
		n.IPV4Addrs = tlv.SomeRecordT(ipv4)
	}

	if _, ok := typeMap[n.IPV6Addrs.TlvType()]; ok {
		n.IPV6Addrs = tlv.SomeRecordT(ipv6)
	}

	if _, ok := typeMap[n.TorV3Addrs.TlvType()]; ok {
		n.TorV3Addrs = tlv.SomeRecordT(torV3)
	}

	if _, ok := typeMap[n.DNSHostName.TlvType()]; ok {
		n.DNSHostName = tlv.SomeRecordT(dns)
	}

	n.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// Encode serializes the target NodeAnnouncement2 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) Encode(w *bytes.Buffer, _ uint32) error {
	return EncodePureTLVMessage(n, w)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (n *NodeAnnouncement2) MsgType() MessageType {
	return MsgNodeAnnouncement2
}

// NodePub returns the identity public key of the node.
//
// NOTE: part of the NodeAnnouncement interface.
func (n *NodeAnnouncement2) NodePub() [33]byte {
	return n.NodeID.Val
}

// NodeFeatures returns the set of features supported by the node.
//
// NOTE: part of the NodeAnnouncement interface.
func (n *NodeAnnouncement2) NodeFeatures() *FeatureVector {
	return NewFeatureVector(&n.Features.Val, Features)
}

// TimestampDesc returns a human-readable description of the timestamp of the
// announcement.
//
// NOTE: part of the NodeAnnouncement interface.
func (n *NodeAnnouncement2) TimestampDesc() string {
	return fmt.Sprintf("block_height=%d", n.BlockHeight.Val)
}

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (n *NodeAnnouncement2) GossipVersion() GossipVersion {
	return GossipVersion2
}

// A compile-time check to ensure NodeAnnouncement2 implements the Message
// interface.
var _ Message = (*NodeAnnouncement2)(nil)

// A compile time check to ensure NodeAnnouncement2 implements the
// lnwire.NodeAnnouncement interface.
var _ NodeAnnouncement = (*NodeAnnouncement2)(nil)

// A compile-time check to ensure NodeAnnouncement2 implements the
// PureTLVMessage interface.
var _ PureTLVMessage = (*NodeAnnouncement2)(nil)

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*NodeAnnouncement2)(nil)

// NodeAlias2 represents a UTF-8 encoded node alias with a maximum length of 32
// bytes.
type NodeAlias2 []byte

// Record returns a TLV record for encoding/decoding NodeAlias2.
func (n *NodeAlias2) Record() tlv.Record {
	sizeFunc := func() uint64 {
		return uint64(len(*n))
	}

	return tlv.MakeDynamicRecord(
		0, n, sizeFunc, nodeAlias2Encoder, nodeAlias2Decoder,
	)
}

// ValidateNodeAlias2 validates that the node alias is valid UTF-8 and within
// the 32-byte limit.
func ValidateNodeAlias2(alias []byte) error {
	if len(alias) == 0 {
		return errors.New("node alias cannot be empty")
	}

	if len(alias) > 32 {
		return fmt.Errorf("node alias exceeds 32 bytes: length %d",
			len(alias))
	}

	if !utf8.Valid(alias) {
		return errors.New("node alias contains invalid UTF-8")
	}

	return nil
}

// nodeAlias2Encoder encodes a NodeAlias2 as raw UTF-8 bytes.
func nodeAlias2Encoder(w io.Writer, val any, _ *[8]byte) error {
	if v, ok := val.(*NodeAlias2); ok {
		if err := ValidateNodeAlias2(*v); err != nil {
			return err
		}

		_, err := w.Write(*v)

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "NodeAlias2")
}

// nodeAlias2Decoder decodes raw bytes into a NodeAlias2.
func nodeAlias2Decoder(r io.Reader, val any, _ *[8]byte, l uint64) error {
	if v, ok := val.(*NodeAlias2); ok {
		if l > 32 {
			return fmt.Errorf("node alias exceeds 32 bytes: %d", l)
		}

		aliasBytes := make([]byte, l)
		if _, err := io.ReadFull(r, aliasBytes); err != nil {
			return err
		}

		if err := ValidateNodeAlias2(aliasBytes); err != nil {
			return err
		}

		*v = aliasBytes

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "NodeAlias2", l, 0)
}

// Color is a custom type to represent a color.RGBA that can be encoded/decoded
// as a TLV record.
type Color color.RGBA

// Record returns a TLV record for encoding/decoding Color.
func (c *Color) Record() tlv.Record {
	return tlv.MakeStaticRecord(0, c, 3, rgbEncoder, rgbDecoder)
}

// rgbEncoder encodes a Color as RGB bytes.
func rgbEncoder(w io.Writer, val any, _ *[8]byte) error {
	if v, ok := val.(*Color); ok {
		buf := bytes.NewBuffer(nil)
		err := WriteColorRGBA(buf, color.RGBA(*v))
		if err != nil {
			return err
		}
		_, err = w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "Color")
}

// rgbDecoder decodes RGB bytes into a Color.
func rgbDecoder(r io.Reader, val any, _ *[8]byte, l uint64) error {
	if v, ok := val.(*Color); ok {
		return ReadElements(r, &v.R, &v.G, &v.B)
	}

	return tlv.NewTypeForDecodingErr(val, "Color", l, 3)
}

// ipv4AddrEncodedSize is the number of bytes required to encode a single ipv4
// address. Four bytes are used to encode the IP address and two bytes for the
// port number.
const ipv4AddrEncodedSize = 4 + 2

// IPV4Addrs is a list of ipv4 addresses that can be encoded as a TLV record.
type IPV4Addrs []*net.TCPAddr

// Record returns a Record that can be used to encode/decode a IPV4Addrs
// to/from a TLV stream.
func (a *IPV4Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, a, a.encodedSize, ipv4AddrsEncoder, ipv4AddrsDecoder,
	)
}

// encodedSize returns the number of bytes required to encode an IPV4Addrs
// variable.
func (a *IPV4Addrs) encodedSize() uint64 {
	return uint64(len(*a) * ipv4AddrEncodedSize)
}

// ipv4AddrsEncoder encodes IPv4 addresses as TLV bytes.
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
			if err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV4Addrs")
}

// ipv4AddrsDecoder decodes TLV bytes into IPv4 addresses.
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

// Record returns a Record that can be used to encode/decode a IPV4Addrs
// to/from a TLV stream.
func (a *IPV6Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, a, a.encodedSize, ipv6AddrsEncoder, ipv6AddrsDecoder,
	)
}

// ipv6AddrEncodedSize is the number of bytes required to encode a single ipv6
// address. Sixteen bytes are used to encode the IP address and two bytes for
// the port number.
const ipv6AddrEncodedSize = 16 + 2

// encodedSize returns the number of bytes required to encode an IPV6Addrs
// variable.
func (a *IPV6Addrs) encodedSize() uint64 {
	return uint64(len(*a) * ipv6AddrEncodedSize)
}

// ipv6AddrsEncoder encodes IPv6 addresses as TLV bytes.
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
			if err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.IPV6Addrs")
}

// ipv6AddrsDecoder decodes TLV bytes into IPv6 addresses.
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

// encodedSize returns the number of bytes required to encode an TorV3Addrs
// variable.
func (a *TorV3Addrs) encodedSize() uint64 {
	return uint64(len(*a) * torV3AddrEncodedSize)
}

// Record returns a Record that can be used to encode/decode a IPV4Addrs
// to/from a TLV stream.
func (a *TorV3Addrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, a, a.encodedSize, torV3AddrsEncoder, torV3AddrsDecoder,
	)
}

// torV3AddrsEncoder encodes Tor v3 addresses as TLV bytes.
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
					tor.V3DecodedLen, len(host))
			}

			if _, err = w.Write(host); err != nil {
				return err
			}

			var port [2]byte
			binary.BigEndian.PutUint16(port[:], uint16(addr.Port))
			if _, err = w.Write(port[:]); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.TorV3Addrs")
}

// torV3AddrsDecoder decodes TLV bytes into Tor v3 addresses.
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

			if len(onionService) != tor.V3Len {
				return fmt.Errorf("expected a tor v3 onion "+
					"service length of %d, got: %d",
					tor.V3Len, len(onionService))
			}

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
