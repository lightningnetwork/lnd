package bolt12

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrTooManyChains is returned when offer_chains declares more entries than
// maxOfferChains.
var ErrTooManyChains = errors.New("offer_chains exceeds maxOfferChains")

// ErrNonMinimalFeatures is returned when a decoded feature vector is not
// canonically (minimally) encoded.
var ErrNonMinimalFeatures = errors.New("non-minimal feature vector encoding")

// ErrTooManyBlindedPayInfos is returned when decoded blinded_payinfo entries
// exceed maxBlindedPayInfos.
var ErrTooManyBlindedPayInfos = errors.New(
	"invoice_blindedpay exceeds maxBlindedPayInfos",
)

// ErrInvalidHtlcRange is returned when a decoded blinded_payinfo entry carries
// an htlc_minimum_msat greater than its htlc_maximum_msat.
var ErrInvalidHtlcRange = errors.New(
	"blinded_payinfo htlc_minimum_msat exceeds htlc_maximum_msat",
)

// ErrTooManyFallbackAddrs is returned when decoded fallback_address entries
// exceed maxFallbackAddrs.
var ErrTooManyFallbackAddrs = errors.New(
	"invoice_fallbacks exceeds maxFallbackAddrs",
)

const (
	// chainHashLen is the length of a chain hash (32 bytes).
	chainHashLen = 32

	// maxOfferChains caps decoded offer_chains entries. This is a sanity
	// check to prevent excessive memory allocation and is not a protocol
	// limit but a local implementation choice.
	maxOfferChains = 32

	// maxBlindedPayInfos caps decoded blinded_payinfo entries to prevent
	// excessive allocation and validation cost.
	maxBlindedPayInfos = 32

	// maxFallbackAddrs caps decoded fallback_address entries to prevent
	// excessive allocation and validation cost.
	maxFallbackAddrs = 32

	// maxFallbackAddrLen bounds the address bytes in a single fallback
	// entry. The spec encodes the length as a uint16, so 65535 is the
	// format's ceiling.
	maxFallbackAddrLen = math.MaxUint16
)

// ChainsRecord holds one or more chain hashes for the offer_chains field.
type ChainsRecord struct {
	Chains [][chainHashLen]byte
}

var _ tlv.RecordProducer = (*ChainsRecord)(nil)

// Record returns a TLV record for ChainsRecord.
func (c *ChainsRecord) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, c,
		func() uint64 {
			return uint64(len(c.Chains)) * chainHashLen
		},
		encodeChainsRecord,
		decodeChainsRecord,
	)
}

// encodeChainsRecord writes the chain hashes in sequence, without a count
// prefix.
func encodeChainsRecord(w io.Writer, val any, _ *[8]byte) error {
	c, ok := val.(*ChainsRecord)
	if !ok {
		return fmt.Errorf("expected *ChainsRecord, got %T", val)
	}

	for _, chain := range c.Chains {
		if _, err := w.Write(chain[:]); err != nil {
			return err
		}
	}

	return nil
}

// decodeChainsRecord caps the count at maxOfferChains to bound allocation.
func decodeChainsRecord(r io.Reader, val any, _ *[8]byte, l uint64) error {
	c, ok := val.(*ChainsRecord)
	if !ok {
		return fmt.Errorf("expected *ChainsRecord, got %T", val)
	}

	if l%chainHashLen != 0 {
		return fmt.Errorf("chains length %d not a multiple of %d", l,
			chainHashLen)
	}

	numChains := l / chainHashLen
	if numChains > maxOfferChains {
		return fmt.Errorf("%w: %d > %d", ErrTooManyChains, numChains,
			maxOfferChains)
	}

	c.Chains = make([][chainHashLen]byte, numChains)
	for i := range c.Chains {
		if _, err := io.ReadFull(r, c.Chains[i][:]); err != nil {
			return err
		}
	}

	return nil
}

// BlindedPayInfo holds the payment parameters for a blinded path, corresponding
// to the blinded_payinfo subtype.
type BlindedPayInfo struct {
	// FeeBaseMsat is the base fee, in millisatoshis, charged for relaying a
	// payment over this blinded path.
	FeeBaseMsat uint32

	// FeeProportionalMillionths is the proportional fee, in millionths of a
	// satoshi per relayed satoshi, charged over this blinded path.
	FeeProportionalMillionths uint32

	// CltvExpiryDelta is the CLTV expiry delta the path requires.
	CltvExpiryDelta uint16

	// HtlcMinimumMsat is the smallest HTLC, in millisatoshis, the path
	// accepts.
	HtlcMinimumMsat uint64

	// HtlcMaximumMsat is the largest HTLC, in millisatoshis, the path
	// accepts.
	HtlcMaximumMsat uint64

	// Features is the relay feature bitmap for this blinded path, typed for
	// consistency with the other BOLT 12 feature fields.
	//
	// WARNING: RawFeatureVector re-encodes to minimal length, so setting
	// non-minimal feature bytes (trailing zeros) yields different wire
	// bytes than were read and invalidates the invoice signature.
	Features lnwire.RawFeatureVector
}

// BlindedPayInfos holds a list of BlindedPayInfo entries for the
// invoice_blindedpay field.
type BlindedPayInfos struct {
	Infos []BlindedPayInfo
}

// Record returns a TLV record for BlindedPayInfos.
//
// NOTE: This implements the tlv.RecordProducer interface.
func (bp *BlindedPayInfos) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, bp,
		func() uint64 {
			return blindedPayInfosSize(bp)
		},
		encodeBlindedPayInfos, decodeBlindedPayInfos,
	)
}

// blindedPayInfosSize returns the encoded byte length of all blinded_payinfo
// entries, used to size the dynamic TLV record.
func blindedPayInfosSize(bp *BlindedPayInfos) uint64 {
	var size uint64
	for _, info := range bp.Infos {
		// fee_base(4) + fee_prop(4) + cltv(2) + htlc_min(8) +
		// htlc_max(8) + flen(2) + features.
		size += 4 + 4 + 2 + 8 + 8 + 2 +
			uint64(info.Features.SerializeSize())
	}

	return size
}

// encodeBlindedPayInfos writes each blinded_payinfo entry in sequence: the
// fixed fee, cltv and htlc fields followed by a u16-length-prefixed feature
// vector. Entries are concatenated without a count prefix; the count is
// recovered on decode from the surrounding invoice_paths length.
func encodeBlindedPayInfos(
	w io.Writer, val interface{}, buf *[8]byte) error {

	bp, ok := val.(*BlindedPayInfos)
	if !ok {
		return fmt.Errorf("expected *BlindedPayInfos, got %T", val)
	}

	for _, info := range bp.Infos {
		binary.BigEndian.PutUint32(buf[:4], info.FeeBaseMsat)
		if _, err := w.Write(buf[:4]); err != nil {
			return err
		}

		binary.BigEndian.PutUint32(
			buf[:4], info.FeeProportionalMillionths,
		)
		if _, err := w.Write(buf[:4]); err != nil {
			return err
		}

		binary.BigEndian.PutUint16(buf[:2], info.CltvExpiryDelta)
		if _, err := w.Write(buf[:2]); err != nil {
			return err
		}

		binary.BigEndian.PutUint64(buf[:8], info.HtlcMinimumMsat)
		if _, err := w.Write(buf[:8]); err != nil {
			return err
		}

		binary.BigEndian.PutUint64(buf[:8], info.HtlcMaximumMsat)
		if _, err := w.Write(buf[:8]); err != nil {
			return err
		}

		// flen is a u16, so guard the cast before framing the minimal
		// feature bytes, mirroring encodeFallbackAddrs.
		flen := info.Features.SerializeSize()
		if flen > math.MaxUint16 {
			return fmt.Errorf("features %d exceed limit %d",
				flen, math.MaxUint16)
		}

		binary.BigEndian.PutUint16(buf[:2], uint16(flen))
		if _, err := w.Write(buf[:2]); err != nil {
			return err
		}
		if err := info.Features.EncodeBase256(w); err != nil {
			return err
		}
	}

	return nil
}

// decodeBlindedPayInfos reads blinded_payinfo entries until the record bytes
// are exhausted. The entry count is capped at maxBlindedPayInfos to prevent
// excessive memory allocation and validation cost.
func decodeBlindedPayInfos(
	r io.Reader, val interface{}, buf *[8]byte, l uint64) error {

	bp, ok := val.(*BlindedPayInfos)
	if !ok {
		return fmt.Errorf("expected *BlindedPayInfos, got %T", val)
	}

	lr := &io.LimitedReader{R: r, N: int64(l)}

	for lr.N > 0 {
		if len(bp.Infos) >= maxBlindedPayInfos {
			return ErrTooManyBlindedPayInfos
		}

		var info BlindedPayInfo

		if _, err := io.ReadFull(lr, buf[:4]); err != nil {
			return fmt.Errorf("read fee_base: %w", err)
		}
		info.FeeBaseMsat = binary.BigEndian.Uint32(buf[:4])

		if _, err := io.ReadFull(lr, buf[:4]); err != nil {
			return fmt.Errorf("read fee_prop: %w", err)
		}
		info.FeeProportionalMillionths = binary.BigEndian.Uint32(
			buf[:4],
		)

		if _, err := io.ReadFull(lr, buf[:2]); err != nil {
			return fmt.Errorf("read cltv_delta: %w", err)
		}
		info.CltvExpiryDelta = binary.BigEndian.Uint16(buf[:2])

		if _, err := io.ReadFull(lr, buf[:8]); err != nil {
			return fmt.Errorf("read htlc_min: %w", err)
		}
		info.HtlcMinimumMsat = binary.BigEndian.Uint64(buf[:8])

		if _, err := io.ReadFull(lr, buf[:8]); err != nil {
			return fmt.Errorf("read htlc_max: %w", err)
		}
		info.HtlcMaximumMsat = binary.BigEndian.Uint64(buf[:8])

		// Defense-in-depth decode check, mirroring the
		// ErrNonMinimalFeatures guard below: reject an inverted HTLC
		// range so the htlc_min <= htlc_max invariant holds for every
		// downstream consumer instead of being re-derived per caller.
		if info.HtlcMinimumMsat > info.HtlcMaximumMsat {
			return ErrInvalidHtlcRange
		}

		// flen then features, mirroring decodeFallbackAddrs: reject a
		// length that overruns the remaining bytes before allocating.
		// Decode into a constructed vector so its map is initialised.
		if _, err := io.ReadFull(lr, buf[:2]); err != nil {
			return fmt.Errorf("read flen: %w", err)
		}
		flen := binary.BigEndian.Uint16(buf[:2])
		if int64(flen) > lr.N {
			return fmt.Errorf("flen %d exceeds remaining %d",
				flen, lr.N)
		}

		fv := lnwire.NewRawFeatureVector()
		if err := fv.DecodeBase256(lr, int(flen)); err != nil {
			return fmt.Errorf("read features: %w", err)
		}
		if fv.SerializeSize() != int(flen) {
			return ErrNonMinimalFeatures
		}
		info.Features = *fv

		bp.Infos = append(bp.Infos, info)
	}

	return nil
}

// FallbackAddress represents an on-chain fallback address.
type FallbackAddress struct {
	Version byte
	Address []byte
}

// FallbackAddresses holds a list of fallback addresses for the
// invoice_fallbacks field.
type FallbackAddresses struct {
	Addrs []FallbackAddress
}

// Record returns a TLV record for FallbackAddresses.
//
// NOTE: This implements the tlv.RecordProducer interface.
func (fa *FallbackAddresses) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, fa,
		func() uint64 {
			return fallbackAddrsSize(fa)
		},
		encodeFallbackAddrs, decodeFallbackAddrs,
	)
}

// fallbackAddrsSize returns the encoded byte length of all fallback_address
// entries, used to size the dynamic TLV record.
func fallbackAddrsSize(fa *FallbackAddresses) uint64 {
	var size uint64
	for _, a := range fa.Addrs {
		// version(1) + len(2) + address
		size += 1 + 2 + uint64(len(a.Address))
	}

	return size
}

// encodeFallbackAddrs writes each fallback_address entry as a version byte, a
// u16 address length and the raw address bytes, concatenated without a count
// prefix.
func encodeFallbackAddrs(
	w io.Writer, val interface{}, buf *[8]byte) error {

	fa, ok := val.(*FallbackAddresses)
	if !ok {
		return fmt.Errorf("expected *FallbackAddresses, got %T", val)
	}

	for i, a := range fa.Addrs {
		if len(a.Address) > maxFallbackAddrLen {
			return fmt.Errorf("fallback %d: address %d exceeds "+
				"limit %d", i, len(a.Address),
				maxFallbackAddrLen)
		}

		buf[0] = a.Version
		if _, err := w.Write(buf[:1]); err != nil {
			return err
		}

		binary.BigEndian.PutUint16(buf[:2], uint16(len(a.Address)))
		if _, err := w.Write(buf[:2]); err != nil {
			return err
		}
		if _, err := w.Write(a.Address); err != nil {
			return err
		}
	}

	return nil
}

// decodeFallbackAddrs reads fallback_address entries until the record bytes are
// exhausted. The entry count is capped at maxFallbackAddrs to prevent
// excessive memory allocation and validation cost.
func decodeFallbackAddrs(
	r io.Reader, val interface{}, buf *[8]byte, l uint64) error {

	fa, ok := val.(*FallbackAddresses)
	if !ok {
		return fmt.Errorf("expected *FallbackAddresses, got %T", val)
	}

	lr := &io.LimitedReader{R: r, N: int64(l)}

	for lr.N > 0 {
		if len(fa.Addrs) >= maxFallbackAddrs {
			return ErrTooManyFallbackAddrs
		}

		var a FallbackAddress

		if _, err := io.ReadFull(lr, buf[:1]); err != nil {
			return fmt.Errorf("read version: %w", err)
		}
		a.Version = buf[0]

		if _, err := io.ReadFull(lr, buf[:2]); err != nil {
			return fmt.Errorf("read addrlen: %w", err)
		}
		addrLen := binary.BigEndian.Uint16(buf[:2])
		if int64(addrLen) > lr.N {
			return fmt.Errorf("addrlen %d exceeds remaining %d",
				addrLen, lr.N)
		}

		a.Address = make([]byte, addrLen)
		if _, err := io.ReadFull(lr, a.Address); err != nil {
			return fmt.Errorf("read address: %w", err)
		}

		fa.Addrs = append(fa.Addrs, a)
	}

	return nil
}
