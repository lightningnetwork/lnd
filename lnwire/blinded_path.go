package lnwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrInvalidIntroNode is returned when a blinded path's introduction
	// node discriminator is not one of the spec-defined values.
	ErrInvalidIntroNode = errors.New("invalid blinded-path introduction " +
		"node discriminator")

	// ErrEmptyBlindedPath is returned when a blinded path has zero hops.
	ErrEmptyBlindedPath = errors.New("blinded path with zero hops")
)

// BlindedPath holds the introduction node, blinding point, and encrypted hops
// of a single blinded path.
type BlindedPath struct {
	// IntroductionNode is the variant-defined introduction node for this
	// blinded path.
	IntroductionNode IntroductionNode

	// BlindingPoint is the blinding point for this path, used to derive the
	// blinded node IDs and encrypt the hop payloads.
	BlindingPoint *btcec.PublicKey

	// Hops is the ordered list of blinded hops in this path.
	Hops []BlindedHop
}

// BlindedPaths holds one or more blinded paths.
type BlindedPaths struct {
	Paths []BlindedPath
}

// BlindedHop represents a single hop in a blinded path.
type BlindedHop struct {
	// BlindedNodeID is the blinded public key for this hop.
	BlindedNodeID *btcec.PublicKey

	// EncryptedData is the encrypted payload for this hop.
	EncryptedData []byte
}

var (
	_ tlv.RecordProducer = (*BlindedPath)(nil)
	_ tlv.RecordProducer = (*BlindedPaths)(nil)
)

// Record returns a TLV record for a single BlindedPath at the BOLT 4 reply_path
// TLV type. Used directly by OnionMessagePayload's reply_path encoding.
func (p *BlindedPath) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		replyPathType, p,
		func() uint64 {
			return blindedPathSize(p)
		},
		encodeBlindedPath,
		decodeBlindedPath,
	)
}

// blindedPathSize returns the on-wire size of a single BlindedPath.
func blindedPathSize(p *BlindedPath) uint64 {
	var introLen uint64
	if p.IntroductionNode != nil {
		introLen = p.IntroductionNode.encodedLen()
	}

	// introduction_node (variant-defined) + blinding_point (33) +
	// num_hops (1).
	size := introLen + pubKeyLen + 1
	for _, h := range p.Hops {
		// blinded_node_id (33) + enclen (2) + enc_data.
		size += pubKeyLen + 2 + uint64(len(h.EncryptedData))
	}

	return size
}

// encodeBlindedPath writes a single blinded path. No bytes are written if the
// path fails validation.
func encodeBlindedPath(w io.Writer, val any, buf *[8]byte) error {
	p, ok := val.(*BlindedPath)
	if !ok {
		return fmt.Errorf("expected *BlindedPath, got %T", val)
	}

	return writeBlindedPath(w, p, buf)
}

// writeBlindedPath validates the path and writes a single blinded path to w.
func writeBlindedPath(w io.Writer, p *BlindedPath, buf *[8]byte) error {
	if p.IntroductionNode == nil {
		return fmt.Errorf("nil intro node")
	}

	if err := p.IntroductionNode.validate(); err != nil {
		return err
	}

	if p.BlindingPoint == nil {
		return fmt.Errorf("nil blinding point")
	}

	if !p.BlindingPoint.IsOnCurve() {
		return fmt.Errorf("blinding point not on curve")
	}

	if len(p.Hops) == 0 {
		return ErrEmptyBlindedPath
	}
	if len(p.Hops) > maxBlindedPathHops {
		return fmt.Errorf("%d hops exceeds limit %d", len(p.Hops),
			maxBlindedPathHops)
	}

	if err := p.IntroductionNode.encode(w); err != nil {
		return err
	}
	blindingBytes := p.BlindingPoint.SerializeCompressed()
	if _, err := w.Write(blindingBytes); err != nil {
		return err
	}

	buf[0] = uint8(len(p.Hops))
	if _, err := w.Write(buf[:1]); err != nil {
		return err
	}

	for hIdx := range p.Hops {
		if err := writeBlindedHop(w, &p.Hops[hIdx], buf); err != nil {
			return fmt.Errorf("hop %d: %w", hIdx, err)
		}
	}

	return nil
}

// decodeBlindedPath reads a single blinded path framed at the TLV-value level.
func decodeBlindedPath(r io.Reader, val any, buf *[8]byte, l uint64) error {
	p, ok := val.(*BlindedPath)
	if !ok {
		return fmt.Errorf("expected *BlindedPath, got %T", val)
	}

	lr := &io.LimitedReader{R: r, N: int64(l)}

	if err := readBlindedPath(lr, p, buf); err != nil {
		return err
	}

	if lr.N != 0 {
		return fmt.Errorf("trailing %d bytes after blinded path", lr.N)
	}

	return nil
}

// readBlindedPath decodes a single blinded path from lr.
func readBlindedPath(lr *io.LimitedReader, p *BlindedPath,
	buf *[8]byte) error {

	intro, err := decodeIntroductionNode(lr, buf)
	if err != nil {
		return err
	}
	p.IntroductionNode = intro

	var blindingBytes [pubKeyLen]byte
	if _, err := io.ReadFull(lr, blindingBytes[:]); err != nil {
		return fmt.Errorf("read blinding point: %w", err)
	}
	blinding, err := btcec.ParsePubKey(blindingBytes[:])
	if err != nil {
		return fmt.Errorf("blinding point: %w", err)
	}
	p.BlindingPoint = blinding

	if _, err := io.ReadFull(lr, buf[:1]); err != nil {
		return fmt.Errorf("read num_hops: %w", err)
	}
	numHops := int(buf[0])
	if numHops == 0 {
		return ErrEmptyBlindedPath
	}

	if int64(numHops)*minBlindedHopBytes > lr.N {
		return fmt.Errorf("num_hops %d exceeds remaining %d bytes",
			numHops, lr.N)
	}

	p.Hops = make([]BlindedHop, numHops)
	for i := range p.Hops {
		if err := readBlindedHop(lr, &p.Hops[i], buf); err != nil {
			return err
		}
	}

	return nil
}

// Record returns a TLV record for BlindedPaths.
func (bp *BlindedPaths) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, bp,
		func() uint64 {
			return blindedPathsSize(bp)
		},
		encodeBlindedPaths,
		decodeBlindedPaths,
	)
}

// blindedPathsSize returns the on-wire size of multiple BlindedPaths.
func blindedPathsSize(bp *BlindedPaths) uint64 {
	var size uint64
	for i := range bp.Paths {
		size += blindedPathSize(&bp.Paths[i])
	}

	return size
}

// encodeBlindedPaths writes the multi-path TLV value as concatenated paths.
// Fails closed under the same conditions as encodeBlindedPath.
func encodeBlindedPaths(w io.Writer, val any, buf *[8]byte) error {
	bp, ok := val.(*BlindedPaths)
	if !ok {
		return fmt.Errorf("expected *BlindedPaths, got %T", val)
	}

	for pIdx := range bp.Paths {
		err := writeBlindedPath(w, &bp.Paths[pIdx], buf)
		if err != nil {
			return fmt.Errorf("blinded path %d: %w", pIdx, err)
		}
	}

	return nil
}

// decodeBlindedPaths reads concatenated blinded paths. The LimitedReader gates
// each variable-length subfield against the bytes still on the wire, so an
// oversize hop count cannot force a large allocation before io.ReadFull
// notices the bytes are absent.
func decodeBlindedPaths(r io.Reader, val any, buf *[8]byte, l uint64) error {
	bp, ok := val.(*BlindedPaths)
	if !ok {
		return fmt.Errorf("expected *BlindedPaths, got %T", val)
	}

	lr := &io.LimitedReader{R: r, N: int64(l)}

	for lr.N > 0 {
		var p BlindedPath
		if err := readBlindedPath(lr, &p, buf); err != nil {
			return err
		}
		bp.Paths = append(bp.Paths, p)
	}

	return nil
}

// writeBlindedHop emits BlindedNodeID + enclen + encrypted data. The size cap
// is checked first so no bytes hit the writer on rejection.
func writeBlindedHop(w io.Writer, h *BlindedHop, buf *[8]byte) error {
	if h.BlindedNodeID == nil {
		return fmt.Errorf("nil blinded node id")
	}

	if !h.BlindedNodeID.IsOnCurve() {
		return fmt.Errorf("blinded node id not on curve")
	}

	if len(h.EncryptedData) > maxEncryptedDataLen {
		return fmt.Errorf("encrypted data %d exceeds limit %d",
			len(h.EncryptedData), maxEncryptedDataLen)
	}

	nodeIDBytes := h.BlindedNodeID.SerializeCompressed()
	if _, err := w.Write(nodeIDBytes); err != nil {
		return err
	}

	binary.BigEndian.PutUint16(buf[:2], uint16(len(h.EncryptedData)))
	if _, err := w.Write(buf[:2]); err != nil {
		return err
	}
	if _, err := w.Write(h.EncryptedData); err != nil {
		return err
	}

	return nil
}

// readBlindedHop decodes a single blinded hop. The enclen guard against lr.N
// bounds the EncryptedData allocation.
func readBlindedHop(lr *io.LimitedReader, h *BlindedHop, buf *[8]byte) error {
	var nodeBytes [pubKeyLen]byte
	if _, err := io.ReadFull(lr, nodeBytes[:]); err != nil {
		return fmt.Errorf("read blinded node: %w", err)
	}
	node, err := btcec.ParsePubKey(nodeBytes[:])
	if err != nil {
		return fmt.Errorf("blinded node id: %w", err)
	}
	h.BlindedNodeID = node

	if _, err := io.ReadFull(lr, buf[:2]); err != nil {
		return fmt.Errorf("read enclen: %w", err)
	}
	encLen := binary.BigEndian.Uint16(buf[:2])
	if int64(encLen) > lr.N {
		return fmt.Errorf("enclen %d exceeds remaining %d", encLen,
			lr.N)
	}

	h.EncryptedData = make([]byte, encLen)
	if _, err := io.ReadFull(lr, h.EncryptedData); err != nil {
		return fmt.Errorf("read encrypted data: %w", err)
	}

	return nil
}
