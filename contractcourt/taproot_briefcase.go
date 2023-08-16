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
	revokeCtrlBlockType       tlv.Type = 1
	outgoingHtlcCtrlBlockType tlv.Type = 2
	incomingHtlcCtrlBlockType tlv.Type = 3
	secondLevelCtrlBlockType  tlv.Type = 4

	anchorTapTweakType                tlv.Type = 0
	htlcTweakCtrlBlockType            tlv.Type = 1
	secondLevelHtlcTweakCtrlBlockType tlv.Type = 2
)

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

// resolverCtrlBlocks is a map of resolver IDs to their corresponding control
// block.
type resolverCtrlBlocks map[resolverID][]byte

// newResolverCtrlBlockss returns a new instance of the resolverCtrlBlocks.
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
func (r *resolverCtrlBlocks) Decode(reader io.Reader) error {
	var buf [8]byte

	numBlocks, err := tlv.ReadVarInt(reader, &buf)
	if err != nil {
		return err
	}

	for i := uint64(0); i < numBlocks; i++ {
		var id resolverID
		if _, err := io.ReadFull(reader, id[:]); err != nil {
			return err
		}

		var ctrlBlock []byte
		err := varBytesDecoder(reader, &ctrlBlock, &buf, 0)
		if err != nil {
			return err
		}

		(*r)[id] = ctrlBlock
	}

	return nil
}

// resolverCtrlBlocksEncoder is a custom TLV encoder for the
// resolverCtrlBlocks.
func resolverCtrlBlocksEncoder(w io.Writer, val any, buf *[8]byte) error {
	if typ, ok := val.(*resolverCtrlBlocks); ok {
		return (*typ).Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "resolverCtrlBlocks")
}

// rsolverCtrlBlocksDecoder is a custom TLV decoder for the resolverCtrlBlocks.
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

	// RevokeSweepCtrlBlock is the serialized control block that's used to
	// sweep the reovked output of a breaching party.
	RevokeSweepCtrlBlock []byte

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

	if len(c.RevokeSweepCtrlBlock) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			revokeCtrlBlockType, &c.RevokeSweepCtrlBlock,
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
		tlv.MakePrimitiveRecord(
			revokeCtrlBlockType, &c.RevokeSweepCtrlBlock,
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

// htlcTapTweakss maps an outpoint (the same format as the resolver ID) to the
// tap tweak needed to sweep a breached HTLC output. This is used for both the
// first and second level HTLC outputs.
type htlcTapTweaks map[resolverID][32]byte

// newHtlcTapTweaks returns a new instance of the htlcTapTweaks struct.
func newHtlcTapTweaks() htlcTapTweaks {
	return make(htlcTapTweaks)
}

// recordSize returns the size of the record in bytes.
func (h *htlcTapTweaks) recordSize() uint64 {
	// Each record will be serialized as: <num_tweaks> || <tweak>, where
	// <tweak> is serialized as: <resolver_key> || <tweak>.
	numTweaks := uint64(len(*h))
	baseSize := tlv.VarIntSize(numTweaks)

	recordSize := baseSize
	for range *h {
		// Each tweak is a fixed 32 bytes, so we just tally that an the
		// size of the resolver ID.
		recordSize += resolverIDLen
		recordSize += 32
	}

	return recordSize
}

// Encode encodes the tap tweaks into the target writer.
func (h *htlcTapTweaks) Encode(w io.Writer) error {
	numTweaks := uint64(len(*h))

	var buf [8]byte
	if err := tlv.WriteVarInt(w, numTweaks, &buf); err != nil {
		return err
	}

	for id, tweak := range *h {
		tweak := tweak

		if _, err := w.Write(id[:]); err != nil {
			return err
		}

		if _, err := w.Write(tweak[:]); err != nil {
			return err
		}
	}

	return nil
}

// htlcTapTweaksEncoder is a custom TLV encoder for the htlcTapTweaks struct.
func htlcTapTweaksEncoder(w io.Writer, val any, buf *[8]byte) error {
	if t, ok := val.(*htlcTapTweaks); ok {
		return (*t).Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "htlcTapTweaks")
}

// htlcTapTweaksDecoder is a custom TLV decoder for the htlcTapTweaks struct.
func htlcTapTweaksDecoder(r io.Reader, val any, buf *[8]byte,
	l uint64) error {

	if typ, ok := val.(*htlcTapTweaks); ok {
		tweakReader := io.LimitReader(r, int64(l))

		htlcTweaks := newHtlcTapTweaks()
		err := htlcTweaks.Decode(tweakReader)
		if err != nil {
			return err
		}

		*typ = htlcTweaks

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "htlcTapTweaks", l, l)
}

// Decode decodes the tap tweaks into the target struct.
func (h *htlcTapTweaks) Decode(reader io.Reader) error {
	var buf [8]byte

	numTweaks, err := tlv.ReadVarInt(reader, &buf)
	if err != nil {
		return err
	}

	for i := uint64(0); i < numTweaks; i++ {
		var id resolverID
		if _, err := io.ReadFull(reader, id[:]); err != nil {
			return err
		}

		var tweak [32]byte
		if _, err := io.ReadFull(reader, tweak[:]); err != nil {
			return err
		}

		(*h)[id] = tweak
	}

	return nil
}

// tapTweaks stores the set of taptweaks needed to perform keyspends for the
// commitment outputs.
type tapTweaks struct {
	// AnchorTweak is the tweak used to derive the key used to spend the
	// anchor output.
	AnchorTweak []byte

	// BreachedHtlcTweaks stores the set of tweaks needed to sweep the
	// revoked first level output of an HTLC.
	BreachedHtlcTweaks htlcTapTweaks

	// BreachedSecondLevelHtlcTweaks stores the set of tweaks needed to
	// sweep the revoked *second* level output of an HTLC.
	BreachedSecondLevelHltcTweaks htlcTapTweaks
}

// newTapTweaks returns a new tapTweaks struct.
func newTapTweaks() *tapTweaks {
	return &tapTweaks{
		BreachedHtlcTweaks:            make(htlcTapTweaks),
		BreachedSecondLevelHltcTweaks: make(htlcTapTweaks),
	}
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

	if len(t.BreachedHtlcTweaks) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			htlcTweakCtrlBlockType, &t.BreachedHtlcTweaks,
			t.BreachedHtlcTweaks.recordSize,
			htlcTapTweaksEncoder, htlcTapTweaksDecoder,
		))
	}

	if len(t.BreachedSecondLevelHltcTweaks) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			secondLevelHtlcTweakCtrlBlockType,
			&t.BreachedSecondLevelHltcTweaks,
			t.BreachedSecondLevelHltcTweaks.recordSize,
			htlcTapTweaksEncoder, htlcTapTweaksDecoder,
		))
	}

	return records
}

// DecodeRecords returns the set of TLV records that decode the tweaks.
func (t *tapTweaks) DecodeRecords() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(anchorTapTweakType, &t.AnchorTweak),
		tlv.MakeDynamicRecord(
			htlcTweakCtrlBlockType, &t.BreachedHtlcTweaks,
			t.BreachedHtlcTweaks.recordSize,
			htlcTapTweaksEncoder, htlcTapTweaksDecoder,
		),
		tlv.MakeDynamicRecord(
			secondLevelHtlcTweakCtrlBlockType,
			&t.BreachedSecondLevelHltcTweaks,
			t.BreachedSecondLevelHltcTweaks.recordSize,
			htlcTapTweaksEncoder, htlcTapTweaksDecoder,
		),
	}
}

// Record returns a TLV record that can be used to encode/decode the tap
// tweaks.
func (t *tapTweaks) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(taprootTapTweakType, t)
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
