package contractcourt

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	taprootCtrlBlockType tlv.Type = 0
	taprootTapTweakType  tlv.Type = 1

	commitCtrlBlockType       tlv.Type = 0
	outgoingHtlcCtrlBlockType tlv.Type = 1
	incomingHtlcCtrlBlockType tlv.Type = 2
	secondLevelCtrlBlockType  tlv.Type = 3

	anchorTapTweakType tlv.Type = 0
)

// tlvDecoder is an interface to support more generic decoding of TLV records.
type tlvDecoder interface {
	// DecodeRecords returns a slice of TLV records that should be decoded.
	DecodeRecords() []tlv.Record

	// Decode decodes the given reader into the target struct.
	Decode(r io.Reader) error
}

// makeAndDecodeStream takes a tlvDecoder and decodes the records encoded in
// the stream into it.
//
// TODO(roasbeef): use elsewhere
func makeAndDecodeStream(r io.Reader, d tlvDecoder) error {
	stream, err := tlv.NewStream(d.DecodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Decode(r)
}

// tlvEncoder is an interface to support more generic encoding of TLV records.
type tlvEncoder interface {
	// EncodeRecords returns a slice of TLV records that should be encoded.
	EncodeRecords() []tlv.Record

	// Encode encodes the target struct into the given writer.
	Encode(w io.Writer) error
}

// makeAnEncodeStream takes a tlvEncoder and encodes the records into the
// target writer as a TLV stream.
func makeAndEncodeStream[E tlvEncoder](w io.Writer, e E) error {
	stream, err := tlv.NewStream(e.EncodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// taprootBriefcase is a supplemental storage struct that contains all the
// information we need to sweep taproot outputs.
type taprootBriefcase struct {
	// CtrlBlock is the set of control block for the taproot outputs.
	CtrlBlocks *ctrlBlocks

	// TapTweaks is the set of taproot tweaks for the taproot outputs that
	// are to be spent via a keyspend path. This includes anchors, and any
	// revocation paths.
	TapTweaks *tapTweaks
}

// newTaprootBriefcase returns a new instance of the taproot specific briefcase
// variant.
func newTaprootBriefcase() *taprootBriefcase {
	return &taprootBriefcase{
		CtrlBlocks: newCtrlBlocks(),
		TapTweaks:  newTapTweaks(),
	}
}

// EncodeRecords returns a slice of TLV records that should be encoded.
func (t *taprootBriefcase) EncodeRecords() []tlv.Record {
	return []tlv.Record{
		newCtrlBlocksRecord(&t.CtrlBlocks),
		newTapTweaksRecord(&t.TapTweaks),
	}
}

// DecodeRecords returns a slice of TLV records that should be decoded.
func (t *taprootBriefcase) DecodeRecords() []tlv.Record {
	return []tlv.Record{
		newCtrlBlocksRecord(&t.CtrlBlocks),
		newTapTweaksRecord(&t.TapTweaks),
	}
}

// Encode records returns a slice of TLV records that should be encoded.
func (t *taprootBriefcase) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(t.EncodeRecords()...)
	if err != nil {
		return err
	}
	return stream.Encode(w)
}

// Decode decodes the given reader into the target struct.
func (t *taprootBriefcase) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(t.DecodeRecords()...)
	if err != nil {
		return err
	}
	return stream.Decode(r)
}

// resolverCtrlBlocks is a map of resolver IDs to their corresponding control block.
type resolverCtrlBlocks map[resolverID][]byte

// newResolverCtrlBlockss returns a new instance of the resolverCtrlBlocks
func newResolverCtrlBlocks() resolverCtrlBlocks {
	return make(resolverCtrlBlocks)
}

// recordSize returns the size of the record in bytes.
func (r *resolverCtrlBlocks) recordSize() uint64 {
	// Each record will be serialized as: <num_total_records> || <record>,
	// where <record> is serialized as: <resolver_key> || <length> ||
	// <ctrl_block>.
	numBlocks := uint64(len(*r))
	baseSize := tlv.VarIntSize(numBlocks)

	recordSize := baseSize
	for _, ctrlBlock := range *r {
		recordSize += resolverIDLen
		recordSize += tlv.VarIntSize(uint64(len(ctrlBlock)))
		recordSize += uint64(len(ctrlBlock))
	}

	return recordSize
}

// Encode encodes the control blocks into the target writer.
func (r *resolverCtrlBlocks) Encode(w io.Writer) error {
	numBlocks := uint64(len(*r))

	var buf [8]byte
	if err := tlv.WriteVarInt(w, numBlocks, &buf); err != nil {
		return err
	}

	for id, ctrlBlock := range *r {
		ctrlBlock := ctrlBlock

		if _, err := w.Write(id[:]); err != nil {
			return err
		}

		if err := varBytesEncoder(w, &ctrlBlock, &buf); err != nil {
			return err
		}
	}

	return nil
}

// Decode decodes the given reader into the target struct.
func (t *resolverCtrlBlocks) Decode(r io.Reader) error {
	var buf [8]byte

	numBlocks, err := tlv.ReadVarInt(r, &buf)
	if err != nil {
		return err
	}

	for i := uint64(0); i < numBlocks; i++ {
		var id resolverID
		if _, err := io.ReadFull(r, id[:]); err != nil {
			return err
		}

		var ctrlBlock []byte
		if err := varBytesDecoder(r, &ctrlBlock, &buf, 0); err != nil {
			return err
		}

		(*t)[id] = ctrlBlock
	}

	return nil
}

// resolverCtrlBlocksEncoder is a custom TLV encoder for the resolverCtrlBlocks
func resolverCtrlBlocksEncoder(w io.Writer, val any, buf *[8]byte) error {
	if typ, ok := val.(*resolverCtrlBlocks); ok {
		return (*typ).Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "resolverCtrlBlocks")
}

// rsolverCtrlBlocksDecoder is a custom TLV decoder for the resolverCtrlBlocks
func resolverCtrlBlocksDecoder(r io.Reader, val any, buf *[8]byte,
	l uint64) error {

	if typ, ok := val.(*resolverCtrlBlocks); ok {
		blockReader := io.LimitReader(r, int64(l))

		resolverBlocks := newResolverCtrlBlocks()
		err := resolverBlocks.Decode(blockReader)
		if err != nil {
			return err
		}

		*typ = resolverBlocks
		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "resolverCtrlBlocks", l, l)
}

// ctrlBlocks is the set of control blocks we need to sweep all the output for
// a taproot/musig2 channel.
type ctrlBlocks struct {
	// CommitSweepCtrlBlock is the serialized control block needed to sweep
	// our commitment output.
	CommitSweepCtrlBlock []byte

	// OutgoingHtlcCtrlBlocks is the set of serialized control blocks for
	// all outgoing HTLCs. This is the set of HTLCs that we offered to the
	// remote party. Depending on which commitment transaction was
	// broadcast, we'll either sweep here and be done, or also need to go
	// to the second level.
	OutgoingHtlcCtrlBlocks resolverCtrlBlocks

	// IncomingHtlcCtrlBlocks is the set of serialized control blocks for
	// all incoming HTLCs
	IncomingHtlcCtrlBlocks resolverCtrlBlocks

	// SecondLevelCtrlBlocks is the set of serialized control blocks for
	// need to sweep the second level HTLCs on our commitment transaction.
	SecondLevelCtrlBlocks resolverCtrlBlocks
}

// newCtrlBlocks returns a new instance of the ctrlBlocks struct.
func newCtrlBlocks() *ctrlBlocks {
	return &ctrlBlocks{
		OutgoingHtlcCtrlBlocks: newResolverCtrlBlocks(),
		IncomingHtlcCtrlBlocks: newResolverCtrlBlocks(),
		SecondLevelCtrlBlocks:  newResolverCtrlBlocks(),
	}
}

// varBytesEncoder is a custom TLV encoder for a variable length byte slice.
func varBytesEncoder(w io.Writer, val any, buf *[8]byte) error {
	if t, ok := val.(*[]byte); ok {
		if err := tlv.WriteVarInt(w, uint64(len(*t)), buf); err != nil {
			return err
		}

		return tlv.EVarBytes(w, t, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "[]byte")
}

// varBytesDecoder is a custom TLV decoder for a variable length byte slice.
func varBytesDecoder(r io.Reader, val any, buf *[8]byte, l uint64) error {
	if typ, ok := val.(*[]byte); ok {
		bytesLen, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		var bytes []byte
		if err := tlv.DVarBytes(r, &bytes, buf, bytesLen); err != nil {
			return err
		}

		*typ = bytes

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "[]byte", l, l)
}

// ctrlBlockEncoder is a custom TLV encoder for the ctrlBlocks struct.
func ctrlBlockEncoder(w io.Writer, val any, buf *[8]byte) error {
	if t, ok := val.(**ctrlBlocks); ok {
		return (*t).Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "ctrlBlocks")
}

// ctrlBlockDecoder is a custom TLV decoder for the ctrlBlocks struct.
func ctrlBlockDecoder(r io.Reader, val any, buf *[8]byte, l uint64) error {
	if typ, ok := val.(**ctrlBlocks); ok {
		ctrlReader := io.LimitReader(r, int64(l))

		var ctrlBlocks ctrlBlocks
		err := ctrlBlocks.Decode(ctrlReader)
		if err != nil {
			return err
		}

		*typ = &ctrlBlocks
		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "ctrlBlocks", l, l)
}

// newCtrlBlocksRecord returns a new TLV record that can be used to
// encode/decode the set of cotrol blocks for the taproot outputs for a
// channel.
func newCtrlBlocksRecord(blks **ctrlBlocks) tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := ctrlBlockEncoder(&b, blks, &buf); err != nil {
			panic(err)
		}
		return uint64(len(b.Bytes()))
	}
	return tlv.MakeDynamicRecord(
		taprootCtrlBlockType, blks, recordSize, ctrlBlockEncoder,
		ctrlBlockDecoder,
	)
}

// EncodeRecords returns the set of TLV records that encode the control block
// for the commitment transaction.
func (c *ctrlBlocks) EncodeRecords() []tlv.Record {
	var records []tlv.Record

	if len(c.CommitSweepCtrlBlock) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			commitCtrlBlockType, &c.CommitSweepCtrlBlock,
		))
	}

	if c.OutgoingHtlcCtrlBlocks != nil {
		records = append(records, tlv.MakeDynamicRecord(
			outgoingHtlcCtrlBlockType, &c.OutgoingHtlcCtrlBlocks,
			c.OutgoingHtlcCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		))
	}

	if c.IncomingHtlcCtrlBlocks != nil {
		records = append(records, tlv.MakeDynamicRecord(
			incomingHtlcCtrlBlockType, &c.IncomingHtlcCtrlBlocks,
			c.IncomingHtlcCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		))
	}

	if c.SecondLevelCtrlBlocks != nil {
		records = append(records, tlv.MakeDynamicRecord(
			secondLevelCtrlBlockType, &c.SecondLevelCtrlBlocks,
			c.SecondLevelCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		))
	}

	return records
}

// DecodeRecords returns the set of TLV records that decode the control block.
func (c *ctrlBlocks) DecodeRecords() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(
			commitCtrlBlockType, &c.CommitSweepCtrlBlock,
		),
		tlv.MakeDynamicRecord(
			outgoingHtlcCtrlBlockType, &c.OutgoingHtlcCtrlBlocks,
			c.OutgoingHtlcCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		),
		tlv.MakeDynamicRecord(
			incomingHtlcCtrlBlockType, &c.IncomingHtlcCtrlBlocks,
			c.IncomingHtlcCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		),
		tlv.MakeDynamicRecord(
			secondLevelCtrlBlockType, &c.SecondLevelCtrlBlocks,
			c.SecondLevelCtrlBlocks.recordSize,
			resolverCtrlBlocksEncoder, resolverCtrlBlocksDecoder,
		),
	}
}

// Record returns a TLV record that can be used to encode/decode the control
// blocks.  type from a given TLV stream.
func (c *ctrlBlocks) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(commitCtrlBlockType, c)
}

// Encode encodes the set of control blocks.
func (c *ctrlBlocks) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(c.EncodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode decodes the set of control blocks.
func (c *ctrlBlocks) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(c.DecodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Decode(r)
}

// tapTweaks stores the set of taptweaks needed to perform keyspends for the
// commitment outputs.
//
// TODO(roasbeef): instead use a map based on resolvers?
type tapTweaks struct {
	// AnchorTweak is the tweak used to derive the key used to spend the anchor output.
	AnchorTweak []byte

	// TODO(roasbeef): breach tweeks
}

// newTapTweaks returns a new tapTweaks struct.
func newTapTweaks() *tapTweaks {
	return &tapTweaks{}
}

// tapTweaksEncoder is a custom TLV encoder for the tapTweaks struct.
func tapTweaksEncoder(w io.Writer, val any, buf *[8]byte) error {
	if t, ok := val.(**tapTweaks); ok {
		return (*t).Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "tapTweaks")
}

// tapTweaksDecoder is a custom TLV decoder for the tapTweaks struct.
func tapTweaksDecoder(r io.Reader, val any, buf *[8]byte, l uint64) error {
	if typ, ok := val.(**tapTweaks); ok {
		tweakReader := io.LimitReader(r, int64(l))

		var tapTweaks tapTweaks
		err := tapTweaks.Decode(tweakReader)
		if err != nil {
			return err
		}

		*typ = &tapTweaks
		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "tapTweaks", l, l)
}

// newTapTweaksRecord returns a new TLV record that can be used to
// encode/decode the tap tweak structs.
func newTapTweaksRecord(tweaks **tapTweaks) tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := tapTweaksEncoder(&b, tweaks, &buf); err != nil {
			panic(err)
		}
		return uint64(len(b.Bytes()))
	}
	return tlv.MakeDynamicRecord(
		taprootTapTweakType, tweaks, recordSize, tapTweaksEncoder,
		tapTweaksDecoder,
	)
}

// EncodeRecords returns the set of TLV records that encode the tweaks.
func (t *tapTweaks) EncodeRecords() []tlv.Record {
	var records []tlv.Record

	if len(t.AnchorTweak) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			anchorTapTweakType, &t.AnchorTweak,
		))
	}

	return records
}

// DecodeRecords returns the set of TLV records that decode the tweaks.
func (t *tapTweaks) DecodeRecords() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(anchorTapTweakType, &t.AnchorTweak),
	}
}

// Record returns a TLV record that can be used to encode/decode the tap
// tweaks.
func (t *tapTweaks) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(anchorTapTweakType, t)
}

// Encode encodes the set of tap tweaks.
func (t *tapTweaks) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(t.EncodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode decodes the set of tap tweaks.
func (t *tapTweaks) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(t.DecodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Decode(r)
}
