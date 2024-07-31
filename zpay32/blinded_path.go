package zpay32

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// maxNumHopsPerPath is the maximum number of blinded path hops that can
	// be included in a single encoded blinded path. This is calculated
	// based on the `data_length` limit of 638 bytes for any tagged field in
	// a BOLT 11 invoice along with the estimated number of bytes required
	// for encoding the most minimal blinded path hop. See the [bLIP
	// proposal](https://github.com/lightning/blips/pull/39) for a detailed
	// calculation.
	maxNumHopsPerPath = 7

	// maxCipherTextLength defines the largest cipher text size allowed.
	// This is derived by using the `data_length` upper bound of 639 bytes
	// and then assuming the case of a path with only a single hop (meaning
	// the cipher text may be as large as possible).
	maxCipherTextLength = 535
)

var (
	// byteOrder defines the endian-ness we use for encoding to and from
	// buffers.
	byteOrder = binary.BigEndian
)

// BlindedPaymentPath holds all the information a payer needs to know about a
// blinded path to a receiver of a payment.
type BlindedPaymentPath struct {
	// FeeBaseMsat is the total base fee for the path in milli-satoshis.
	FeeBaseMsat uint32

	// FeeRate is the total fee rate for the path in parts per million.
	FeeRate uint32

	// CltvExpiryDelta is the total CLTV delta to apply to the path.
	CltvExpiryDelta uint16

	// HTLCMinMsat is the minimum number of milli-satoshis that any hop in
	// the path will route.
	HTLCMinMsat uint64

	// HTLCMaxMsat is the maximum number of milli-satoshis that a hop in the
	// path will route.
	HTLCMaxMsat uint64

	// Features is the feature bit vector for the path.
	Features *lnwire.FeatureVector

	// FirstEphemeralBlindingPoint is the blinding point to send to the
	// introduction node. It will be used by the introduction node to derive
	// a shared secret with the receiver which can then be used to decode
	// the encrypted payload from the receiver.
	FirstEphemeralBlindingPoint *btcec.PublicKey

	// Hops is the blinded path. The first hop is the introduction node and
	// so the BlindedNodeID of this hop will be the real node ID.
	Hops []*sphinx.BlindedHopInfo
}

// DecodeBlindedPayment attempts to parse a BlindedPaymentPath from the passed
// reader.
func DecodeBlindedPayment(r io.Reader) (*BlindedPaymentPath, error) {
	var payment BlindedPaymentPath

	if err := binary.Read(r, byteOrder, &payment.FeeBaseMsat); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &payment.FeeRate); err != nil {
		return nil, err
	}

	err := binary.Read(r, byteOrder, &payment.CltvExpiryDelta)
	if err != nil {
		return nil, err
	}

	err = binary.Read(r, byteOrder, &payment.HTLCMinMsat)
	if err != nil {
		return nil, err
	}

	err = binary.Read(r, byteOrder, &payment.HTLCMaxMsat)
	if err != nil {
		return nil, err
	}

	// Parse the feature bit vector.
	f := lnwire.EmptyFeatureVector()
	err = f.Decode(r)
	if err != nil {
		return nil, err
	}
	payment.Features = f

	// Parse the first ephemeral blinding point.
	var blindingPointBytes [btcec.PubKeyBytesLenCompressed]byte
	_, err = r.Read(blindingPointBytes[:])
	if err != nil {
		return nil, err
	}

	blinding, err := btcec.ParsePubKey(blindingPointBytes[:])
	if err != nil {
		return nil, err
	}
	payment.FirstEphemeralBlindingPoint = blinding

	// Read the one byte hop number.
	var numHops [1]byte
	_, err = r.Read(numHops[:])
	if err != nil {
		return nil, err
	}

	payment.Hops = make([]*sphinx.BlindedHopInfo, int(numHops[0]))

	// Parse each hop.
	for i := 0; i < len(payment.Hops); i++ {
		hop, err := DecodeBlindedHop(r)
		if err != nil {
			return nil, err
		}

		payment.Hops[i] = hop
	}

	return &payment, nil
}

// Encode serialises the BlindedPaymentPath and writes the bytes to the passed
// writer.
// 1) The first 26 bytes contain the relay info:
//   - Base Fee in msat: uint32 (4 bytes).
//   - Proportional Fee in PPM: uint32 (4 bytes).
//   - CLTV expiry delta: uint16 (2 bytes).
//   - HTLC min msat: uint64 (8 bytes).
//   - HTLC max msat: uint64 (8 bytes).
//
// 2) Feature bit vector length (2 bytes).
// 3) Feature bit vector (can be zero length).
// 4) First blinding point: 33 bytes.
// 5) Number of hops: 1 byte.
// 6) Encoded BlindedHops.
func (p *BlindedPaymentPath) Encode(w io.Writer) error {
	if err := binary.Write(w, byteOrder, p.FeeBaseMsat); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.FeeRate); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.CltvExpiryDelta); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.HTLCMinMsat); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.HTLCMaxMsat); err != nil {
		return err
	}

	if err := p.Features.Encode(w); err != nil {
		return err
	}

	_, err := w.Write(p.FirstEphemeralBlindingPoint.SerializeCompressed())
	if err != nil {
		return err
	}

	numHops := len(p.Hops)
	if numHops > maxNumHopsPerPath {
		return fmt.Errorf("the number of hops, %d, exceeds the "+
			"maximum of %d", numHops, maxNumHopsPerPath)
	}

	if _, err := w.Write([]byte{byte(numHops)}); err != nil {
		return err
	}

	for _, hop := range p.Hops {
		if err := EncodeBlindedHop(w, hop); err != nil {
			return err
		}
	}

	return nil
}

// DecodeBlindedHop reads a sphinx.BlindedHopInfo from the passed reader.
func DecodeBlindedHop(r io.Reader) (*sphinx.BlindedHopInfo, error) {
	var nodeIDBytes [btcec.PubKeyBytesLenCompressed]byte
	_, err := r.Read(nodeIDBytes[:])
	if err != nil {
		return nil, err
	}

	nodeID, err := btcec.ParsePubKey(nodeIDBytes[:])
	if err != nil {
		return nil, err
	}

	dataLen, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return nil, err
	}

	if dataLen > maxCipherTextLength {
		return nil, fmt.Errorf("a blinded hop cipher text blob may "+
			"not exceed the maximum of %d bytes",
			maxCipherTextLength)
	}

	encryptedData := make([]byte, dataLen)
	_, err = r.Read(encryptedData)
	if err != nil {
		return nil, err
	}

	return &sphinx.BlindedHopInfo{
		BlindedNodePub: nodeID,
		CipherText:     encryptedData,
	}, nil
}

// EncodeBlindedHop writes the passed BlindedHopInfo to the given writer.
//
// 1) Blinded node pub key: 33 bytes
// 2) Cipher text length: BigSize
// 3) Cipher text.
func EncodeBlindedHop(w io.Writer, hop *sphinx.BlindedHopInfo) error {
	_, err := w.Write(hop.BlindedNodePub.SerializeCompressed())
	if err != nil {
		return err
	}

	if len(hop.CipherText) > maxCipherTextLength {
		return fmt.Errorf("encrypted recipient data can not exceed a "+
			"length of %d bytes", maxCipherTextLength)
	}

	err = tlv.WriteVarInt(w, uint64(len(hop.CipherText)), &[8]byte{})
	if err != nil {
		return err
	}

	_, err = w.Write(hop.CipherText)

	return err
}
