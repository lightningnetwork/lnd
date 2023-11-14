package channeldb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

// edgeInfoEncodingType indicate how the bytes for a channel edge have been
// serialised.
type edgeInfoEncodingType uint8

const (
	// edgeInfo2EncodingType will be used as a prefix for edge's advertised
	// using the ChannelAnnouncement2 message. The type indicates how the
	// bytes following should be deserialized.
	edgeInfo2EncodingType edgeInfoEncodingType = 0
)

const (
	// EdgeInfo2MsgType is the tlv type used within the serialisation of
	// ChannelEdgeInfo2 for storing the serialisation of the associated
	// lnwire.ChannelAnnouncement2 message.
	EdgeInfo2MsgType = tlv.Type(0)

	// EdgeInfo2ChanPoint is the tlv type used within the serialisation of
	// ChannelEdgeInfo2 for storing channel point.
	EdgeInfo2ChanPoint = tlv.Type(1)

	// EdgeInfo2Sig is the tlv type used within the serialisation of
	// ChannelEdgeInfo2 for storing the signature of the
	// lnwire.ChannelAnnouncement2 message.
	EdgeInfo2Sig = tlv.Type(2)
)

const (
	// chanEdgeNewEncodingPrefix is a byte used in the channel edge encoding
	// to signal that the new style encoding which is prefixed with a type
	// byte is being used instead of the legacy encoding which would start
	// with either 0x02 or 0x03 due to the fact that the encoding would
	// start with a node's compressed public key.
	chanEdgeNewEncodingPrefix = 0xff
)

// putChanEdgeInfo serialises the given ChannelEdgeInfo and writes the result
// to the edgeIndex using the channel ID as a key.
func putChanEdgeInfo(edgeIndex kvdb.RwBucket,
	edgeInfo models.ChannelEdgeInfo) error {

	var (
		chanID [8]byte
		b      bytes.Buffer
	)

	binary.BigEndian.PutUint64(chanID[:], edgeInfo.GetChanID())

	if err := serializeChanEdgeInfo(&b, edgeInfo); err != nil {
		return err
	}

	return edgeIndex.Put(chanID[:], b.Bytes())
}

func serializeChanEdgeInfo(w io.Writer, edgeInfo models.ChannelEdgeInfo) error {
	var (
		withTypeByte bool
		typeByte     edgeInfoEncodingType
		serialize    func(w io.Writer) error
	)

	switch info := edgeInfo.(type) {
	case *models.ChannelEdgeInfo1:
		serialize = func(w io.Writer) error {
			return serializeChanEdgeInfo1(w, info)
		}
	case *models.ChannelEdgeInfo2:
		withTypeByte = true
		typeByte = edgeInfo2EncodingType

		serialize = func(w io.Writer) error {
			return serializeChanEdgeInfo2(w, info)
		}
	default:
		return fmt.Errorf("unhandled implementation of "+
			"ChannelEdgeInfo: %T", edgeInfo)
	}

	if withTypeByte {
		// First, write the identifying encoding byte to signal that
		// this is not using the legacy encoding.
		_, err := w.Write([]byte{chanEdgeNewEncodingPrefix})
		if err != nil {
			return err
		}

		// Now, write the encoding type.
		_, err = w.Write([]byte{byte(typeByte)})
		if err != nil {
			return err
		}
	}

	return serialize(w)
}

func serializeChanEdgeInfo1(w io.Writer,
	edgeInfo *models.ChannelEdgeInfo1) error {

	if _, err := w.Write(edgeInfo.NodeKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := w.Write(edgeInfo.NodeKey2Bytes[:]); err != nil {
		return err
	}
	if _, err := w.Write(edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := w.Write(edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, edgeInfo.Features); err != nil {
		return err
	}

	authProof := edgeInfo.AuthProof
	var nodeSig1, nodeSig2, bitcoinSig1, bitcoinSig2 []byte
	if authProof != nil {
		nodeSig1 = authProof.NodeSig1Bytes
		nodeSig2 = authProof.NodeSig2Bytes
		bitcoinSig1 = authProof.BitcoinSig1Bytes
		bitcoinSig2 = authProof.BitcoinSig2Bytes
	}

	if err := wire.WriteVarBytes(w, 0, nodeSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, nodeSig2); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, bitcoinSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, bitcoinSig2); err != nil {
		return err
	}

	if err := writeOutpoint(w, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	err := binary.Write(w, byteOrder, uint64(edgeInfo.Capacity))
	if err != nil {
		return err
	}

	var chanID [8]byte
	binary.BigEndian.PutUint64(chanID[:], edgeInfo.ChannelID)
	if _, err := w.Write(chanID[:]); err != nil {
		return err
	}
	if _, err := w.Write(edgeInfo.ChainHash[:]); err != nil {
		return err
	}

	if len(edgeInfo.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edgeInfo.ExtraOpaqueData))
	}

	return wire.WriteVarBytes(w, 0, edgeInfo.ExtraOpaqueData)
}

func serializeChanEdgeInfo2(w io.Writer, edge *models.ChannelEdgeInfo2) error {
	if len(edge.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edge.ExtraOpaqueData))
	}

	serializedMsg, err := edge.DataToSign()
	if err != nil {
		return err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(EdgeInfo2MsgType, &serializedMsg),
		tlv.MakeStaticRecord(
			EdgeInfo2ChanPoint, &edge.ChannelPoint, 34,
			encodeOutpoint, decodeOutpoint,
		),
	}

	if edge.AuthProof != nil {
		records = append(
			records,
			tlv.MakePrimitiveRecord(
				EdgeInfo2Sig, &edge.AuthProof.SchnorrSigBytes,
			),
		)
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

func fetchChanEdgeInfo(edgeIndex kvdb.RBucket,
	chanID []byte) (models.ChannelEdgeInfo, error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return nil, ErrEdgeNotFound
	}

	edgeInfoReader := bytes.NewReader(edgeInfoBytes)

	return deserializeChanEdgeInfo(edgeInfoReader)
}

func deserializeChanEdgeInfo(reader io.Reader) (models.ChannelEdgeInfo, error) {
	// Wrap the io.Reader in a bufio.Reader so that we can peak the first
	// byte of the stream without actually consuming from the stream.
	r := bufio.NewReader(reader)

	firstByte, err := r.Peek(1)
	if err != nil {
		return nil, err
	}

	if firstByte[0] != chanEdgeNewEncodingPrefix {
		return deserializeChanEdgeInfo1(r)
	}

	// Pop the encoding type byte.
	var scratch [1]byte
	if _, err = r.Read(scratch[:]); err != nil {
		return nil, err
	}

	// Now, read the encoding type byte.
	if _, err = r.Read(scratch[:]); err != nil {
		return nil, err
	}

	encoding := edgeInfoEncodingType(scratch[0])
	switch encoding {
	case edgeInfo2EncodingType:
		return deserializeChanEdgeInfo2(r)

	default:
		return nil, fmt.Errorf("unknown edge info encoding type: %d",
			encoding)
	}
}

func deserializeChanEdgeInfo1(r io.Reader) (*models.ChannelEdgeInfo1, error) {
	var (
		err      error
		edgeInfo models.ChannelEdgeInfo1
	)

	if _, err := io.ReadFull(r, edgeInfo.NodeKey1Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.NodeKey2Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return nil, err
	}

	edgeInfo.Features, err = wire.ReadVarBytes(r, 0, 900, "features")
	if err != nil {
		return nil, err
	}

	var proof models.ChannelAuthProof1

	proof.NodeSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.NodeSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}

	if !proof.IsEmpty() {
		edgeInfo.AuthProof = &proof
	}

	edgeInfo.ChannelPoint = wire.OutPoint{}
	if err := readOutpoint(r, &edgeInfo.ChannelPoint); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.Capacity); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.ChannelID); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, edgeInfo.ChainHash[:]); err != nil {
		return nil, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edgeInfo.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, err
	}

	return &edgeInfo, nil
}

func deserializeChanEdgeInfo2(r io.Reader) (*models.ChannelEdgeInfo2, error) {
	var (
		edgeInfo models.ChannelEdgeInfo2
		msgBytes []byte
		sigBytes []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(EdgeInfo2MsgType, &msgBytes),
		tlv.MakeStaticRecord(
			EdgeInfo2ChanPoint, &edgeInfo.ChannelPoint, 34,
			encodeOutpoint, decodeOutpoint,
		),
		tlv.MakePrimitiveRecord(EdgeInfo2Sig, &sigBytes),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	typeMap, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(msgBytes)
	err = edgeInfo.ChannelAnnouncement2.DecodeTLVRecords(reader)
	if err != nil {
		return nil, err
	}

	if _, ok := typeMap[EdgeInfo2Sig]; ok {
		edgeInfo.AuthProof = &models.ChannelAuthProof2{
			SchnorrSigBytes: sigBytes,
		}
	}

	return &edgeInfo, nil
}

func encodeOutpoint(w io.Writer, val interface{}, _ *[8]byte) error {
	if o, ok := val.(*wire.OutPoint); ok {
		return writeOutpoint(w, o)
	}

	return tlv.NewTypeForEncodingErr(val, "*wire.Outpoint")
}

func decodeOutpoint(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if o, ok := val.(*wire.OutPoint); ok {
		return readOutpoint(r, o)
	}

	return tlv.NewTypeForDecodingErr(val, "*wire.Outpoint", l, l)
}
