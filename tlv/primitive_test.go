package tlv_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

var testPK, _ = btcec.ParsePubKey([]byte{0x02,
	0x3d, 0xa0, 0x92, 0xf6, 0x98, 0x0e, 0x58, 0xd2,
	0xc0, 0x37, 0x17, 0x31, 0x80, 0xe9, 0xa4, 0x65,
	0x47, 0x60, 0x26, 0xee, 0x50, 0xf9, 0x66, 0x95,
	0x96, 0x3e, 0x8e, 0xfe, 0x43, 0x6f, 0x54, 0xeb,
})

type primitive struct {
	u8      byte
	u16     uint16
	u32     uint32
	u64     uint64
	b32     [32]byte
	b33     [33]byte
	b64     [64]byte
	pk      *btcec.PublicKey
	bytes   []byte
	boolean bool
}

// TestWrongEncodingType asserts that all primitives encoders will fail with a
// ErrTypeForEncoding when an incorrect type is provided.
func TestWrongEncodingType(t *testing.T) {
	encoders := []tlv.Encoder{
		tlv.EUint8,
		tlv.EUint16,
		tlv.EUint32,
		tlv.EUint64,
		tlv.EBytes32,
		tlv.EBytes33,
		tlv.EBytes64,
		tlv.EPubKey,
		tlv.EVarBytes,
		tlv.EBool,
	}

	// We'll use an int32 since it is not a primitive type, which should
	// cause the primitive encoders to fail with an ErrTypeForEncoding
	// failure.
	var (
		value int32
		buf   [8]byte
		b     bytes.Buffer
	)
	for _, encoder := range encoders {
		err := encoder(&b, &value, &buf)
		if _, ok := err.(tlv.ErrTypeForEncoding); !ok {
			t.Fatalf("expected error of type ErrTypeForEncoding, "+
				"got %T", err)
		}
	}
}

// TestWrongDecodingType asserts that all primitives decoders will fail with a
// ErrTypeForDecoding when an incorrect type is provided.
func TestWrongDecodingType(t *testing.T) {
	decoders := []tlv.Decoder{
		tlv.DUint8,
		tlv.DUint16,
		tlv.DUint32,
		tlv.DUint64,
		tlv.DBytes32,
		tlv.DBytes33,
		tlv.DBytes64,
		tlv.DPubKey,
		tlv.DVarBytes,
		tlv.DBool,
	}

	// We'll use an int32 since it is not a primitive type, which should
	// cause the primitive decoders to fail with an ErrTypeForDecoding
	// failure.
	var (
		value int32
		buf   [8]byte
		b     bytes.Buffer
	)
	for _, decoder := range decoders {
		err := decoder(&b, &value, &buf, 0)
		if _, ok := err.(tlv.ErrTypeForDecoding); !ok {
			t.Fatalf("expected error of type ErrTypeForDecoding, "+
				"got %T", err)
		}
	}
}

type fieldEncoder struct {
	val     interface{}
	encoder tlv.Encoder
}

type fieldDecoder struct {
	val     interface{}
	decoder tlv.Decoder
	size    uint64
}

// TestPrimitiveEncodings tests that we are able to serialize all known
// primitive field types. After successfully encoding, we check that we are able
// to decode the output and arrive at the same fields.
func TestPrimitiveEncodings(t *testing.T) {
	prim := primitive{
		u8:      0x01,
		u16:     0x0201,
		u32:     0x02000001,
		u64:     0x0200000000000001,
		b32:     [32]byte{0x02, 0x01},
		b33:     [33]byte{0x03, 0x01},
		b64:     [64]byte{0x02, 0x01},
		pk:      testPK,
		bytes:   []byte{0xaa, 0xbb},
		boolean: true,
	}

	encoders := []fieldEncoder{
		{
			val:     &prim.u8,
			encoder: tlv.EUint8,
		},
		{
			val:     &prim.u16,
			encoder: tlv.EUint16,
		},
		{
			val:     &prim.u32,
			encoder: tlv.EUint32,
		},
		{
			val:     &prim.u64,
			encoder: tlv.EUint64,
		},
		{
			val:     &prim.b32,
			encoder: tlv.EBytes32,
		},
		{
			val:     &prim.b33,
			encoder: tlv.EBytes33,
		},
		{
			val:     &prim.b64,
			encoder: tlv.EBytes64,
		},
		{
			val:     &prim.pk,
			encoder: tlv.EPubKey,
		},
		{
			val:     &prim.bytes,
			encoder: tlv.EVarBytes,
		},
		{
			val:     &prim.boolean,
			encoder: tlv.EBool,
		},
	}

	// First we'll encode the primitive fields into a buffer.
	var (
		b   bytes.Buffer
		buf [8]byte
	)
	for _, field := range encoders {
		err := field.encoder(&b, field.val, &buf)
		if err != nil {
			t.Fatalf("unable to encode %T: %v",
				field.val, err)
		}
	}

	// Next, we'll attempt to decode the primitive fields into a separate
	// primitive struct.
	r := bytes.NewReader(b.Bytes())
	var prim2 primitive

	decoders := []fieldDecoder{
		{
			val:     &prim2.u8,
			decoder: tlv.DUint8,
			size:    1,
		},
		{
			val:     &prim2.u16,
			decoder: tlv.DUint16,
			size:    2,
		},
		{
			val:     &prim2.u32,
			decoder: tlv.DUint32,
			size:    4,
		},
		{
			val:     &prim2.u64,
			decoder: tlv.DUint64,
			size:    8,
		},
		{
			val:     &prim2.b32,
			decoder: tlv.DBytes32,
			size:    32,
		},
		{
			val:     &prim2.b33,
			decoder: tlv.DBytes33,
			size:    33,
		},
		{
			val:     &prim2.b64,
			decoder: tlv.DBytes64,
			size:    64,
		},
		{
			val:     &prim2.pk,
			decoder: tlv.DPubKey,
			size:    33,
		},
		{
			val:     &prim2.bytes,
			decoder: tlv.DVarBytes,
			size:    2,
		},
		{
			val:     &prim2.boolean,
			decoder: tlv.DBool,
			size:    1,
		},
	}

	for _, field := range decoders {
		err := field.decoder(r, field.val, &buf, field.size)
		if err != nil {
			t.Fatalf("unable to decode %T: %v",
				field.val, err)
		}
	}

	// Finally, we'll compare that the original and the decode structs are
	// equal.
	if !reflect.DeepEqual(prim, prim2) {
		t.Fatalf("primitive mismatch, "+
			"expected: %v, got: %v",
			prim, prim2)
	}
}
