package bolt12

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// offerStringSeeds returns the bolt12 strings from offers-test.json. Every
// spec-compliant string becomes a corpus entry the mutator can branch from,
// reaching code paths randomly drawn bytes never reach.
func offerStringSeeds(t testing.TB) []string {
	t.Helper()

	var seeds []string
	for _, v := range loadOffersVectors(t) {
		if v.Bolt12 != "" {
			seeds = append(seeds, v.Bolt12)
		}
	}

	return seeds
}

// invreqStringSeeds returns the bolt12 strings from signature-test.json, which
// exercise the invoice_request format.
func invreqStringSeeds(t testing.TB) []string {
	t.Helper()

	var seeds []string
	for _, v := range loadSignatureVectors(t) {
		if v.Bolt12 != "" {
			seeds = append(seeds, v.Bolt12)
		}
	}

	return seeds
}

// tlvStreams bech32-decodes each string and collects the TLV payloads, skipping
// strings that fail to decode. Byte-level decoders get the same corpus benefit
// as the string decoders without routing through bech32.
func tlvStreams(t testing.TB, strings []string) [][]byte {
	t.Helper()

	var seeds [][]byte
	for _, s := range strings {
		_, tlvBytes, err := Decode(s)
		if err != nil {
			continue
		}
		seeds = append(seeds, tlvBytes)
	}

	return seeds
}

// offerTLVSeeds returns the TLV streams behind the offers-test.json strings.
func offerTLVSeeds(t testing.TB) [][]byte {
	t.Helper()

	return tlvStreams(t, offerStringSeeds(t))
}

// invreqTLVSeeds returns the TLV streams behind the signature-test.json invoice
// request strings.
func invreqTLVSeeds(t testing.TB) [][]byte {
	t.Helper()

	return tlvStreams(t, invreqStringSeeds(t))
}

// byteCodec constrains PM to *M with an Encode method, the shape every message
// decoder returns. The pointer core type makes PM nilable, so the harness can
// compare a decoded message against nil. A method-only constraint would admit
// non-pointer types and the check would not compile.
type byteCodec[M any] interface {
	*M
	Encode() ([]byte, error)
}

// fuzzByteCodec registers a byte-level decode harness on f. Decode must never
// panic, and a nil message with nil error is fatal. A decoded message that
// passes writer validation must round-trip encode→decode→encode
// byte-identically.
func fuzzByteCodec[M any, PM byteCodec[M]](f *testing.F,
	decode func([]byte) (PM, error), seeds ...[]byte) {

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := decode(data)
		if err != nil {
			return
		}
		if msg == nil {
			t.Fatal("nil message with nil error")
		}

		encoded, err := msg.Encode()
		if err != nil {
			// Read accepts constraints write rejects, so a decoded
			// message may fail writer validation. Skip the
			// round-trip then.
			return
		}

		again, err := decode(encoded)
		if err != nil {
			t.Fatalf("round-trip decode failed: %v", err)
		}
		encoded2, err := again.Encode()
		if err != nil {
			t.Fatalf("second encode failed: %v", err)
		}
		if !bytes.Equal(encoded, encoded2) {
			t.Fatal("round-trip changed encoded bytes")
		}
	})
}

// fuzzStringCodec registers a bech32 string decode harness on f: decoding any
// mutated string must return cleanly, never panic.
func fuzzStringCodec(f *testing.F, decode func(string), seeds ...string) {
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		decode(s)
	})
}

// FuzzDecodeOffer fuzzes decodeOffer with offers-test.json corpus seeds. Decode
// must never panic and valid decodes round-trip byte-identically.
func FuzzDecodeOffer(f *testing.F) {
	fuzzByteCodec(f, decodeOffer, offerTLVSeeds(f)...)
}

// FuzzDecodeInvoiceRequest fuzzes DecodeInvoiceRequest with signature-test.json
// corpus seeds. Decode must never panic and valid decodes round-trip
// byte-identically.
func FuzzDecodeInvoiceRequest(f *testing.F) {
	fuzzByteCodec(f, DecodeInvoiceRequest, invreqTLVSeeds(f)...)
}

// FuzzDecodeInvoice fuzzes DecodeInvoice with a minimal type-168 seed. Decode
// must never panic and valid decodes round-trip byte-identically.
func FuzzDecodeInvoice(f *testing.F) {
	fuzzByteCodec(f, DecodeInvoice, []byte{
		0xa8, 0x20, // type=168, length=32
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	})
}

// FuzzDecodeInvoiceError fuzzes DecodeInvoiceError with a type-5 error seed.
// Decode must never panic and valid decodes round-trip byte-identically.
func FuzzDecodeInvoiceError(f *testing.F) {
	fuzzByteCodec(f, DecodeInvoiceError, []byte{
		0x05, 0x05, // type=5 error, length=5
		'h', 'e', 'l', 'l', 'o',
	})
}

// FuzzDecodeOfferString exercises the offer bech32 wrapper, reader gates
// included.
func FuzzDecodeOfferString(f *testing.F) {
	fuzzStringCodec(f, func(s string) {
		_, _ = DecodeOfferString(
			s, farFutureNow(), bitcoinMainnetGenesisHash,
		)
	}, offerStringSeeds(f)...)
}

// FuzzDecodeInvoiceRequestString exercises the invoice request bech32 wrapper,
// reader gates included.
func FuzzDecodeInvoiceRequestString(f *testing.F) {
	fuzzStringCodec(f, func(s string) {
		_, _ = DecodeInvoiceRequestString(
			s, bitcoinMainnetGenesisHash,
		)
	}, invreqStringSeeds(f)...)
}

// FuzzDecodeInvoiceString exercises the invoice bech32 wrapper, reader gates
// included. The fixed clock sits one second after the seed invoice's creation
// time so the seed passes the expiry gate and exercises the full decode path.
func FuzzDecodeInvoiceString(f *testing.F) {
	priv, _ := bobKey()
	inv := validInvoice(f)

	sig, err := SignInvoice(inv, priv)
	require.NoError(f, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	seed, err := EncodeInvoiceString(inv)
	require.NoError(f, err)

	fuzzStringCodec(f, func(s string) {
		_, _ = DecodeInvoiceString(
			s, time.Unix(1234567890+1, 0),
			bitcoinMainnetGenesisHash,
		)
	}, seed)
}

// FuzzBech32RoundTrip pins the Encode/Decode bijection on the bech32 layer
// alone, decoupled from any TLV-level concerns. The mutator can permute HRP,
// length, and bytes. Any input that round-trips successfully must yield the
// original (hrp, data) pair.
func FuzzBech32RoundTrip(f *testing.F) {
	f.Add(uint8(0), []byte{0x00})
	f.Add(uint8(1), []byte{0xab, 0xcd, 0xef})
	f.Add(uint8(2), bytes.Repeat([]byte{0x42}, 256))

	hrps := []string{HRPOffer, HRPInvoiceRequest, HRPInvoice}

	f.Fuzz(func(t *testing.T, hrpIdx uint8, data []byte) {
		if len(data) == 0 {
			return
		}

		hrp := hrps[int(hrpIdx)%len(hrps)]
		encoded, err := Encode(hrp, data)
		if err != nil {
			return
		}

		gotHRP, gotData, err := Decode(encoded)
		if err != nil {
			t.Fatalf(
				"decode after successful encode "+
					"failed: %v", err,
			)
		}
		if gotHRP != hrp {
			t.Fatalf(
				"hrp mismatch: encoded with %q, "+
					"decoded as %q", hrp, gotHRP,
			)
		}
		if !bytes.Equal(gotData, data) {
			t.Fatalf(
				"data mismatch: input %s, output %s",
				hex.EncodeToString(data),
				hex.EncodeToString(gotData),
			)
		}
	})
}

// FuzzMerkleRootDeterminism asserts merkleRoot is deterministic: the root is
// recomputed independently at sign and verify time, so any nondeterminism
// silently breaks signature reproducibility. Two calls over identical records
// must return identical roots. The parse uses skip-on-error semantics, so it
// cannot reuse streamToRecords, which fails the test on malformed input.
func FuzzMerkleRootDeterminism(f *testing.F) {
	seeds := append(offerTLVSeeds(f), invreqTLVSeeds(f)...)
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		stream, err := tlv.NewStream()
		require.NoError(t, err)

		typeMap, err := stream.DecodeWithParsedTypesP2P(
			bytes.NewReader(data),
		)

		// The test pins determinism, not TLV validity, so inputs that
		// yield no records are skipped.
		if err != nil || len(typeMap) == 0 {
			return
		}

		records := lnwire.TlvMapToRecords(typeMap)

		root1, err := merkleRoot(records)
		if err != nil {
			return
		}

		root2, err := merkleRoot(records)
		if err != nil {
			t.Fatalf("second merkleRoot call failed: %v", err)
		}

		if root1 != root2 {
			t.Fatal("merkleRoot returned different roots for " +
				"identical records")
		}
	})
}
