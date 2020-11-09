package tlv_test

import (
	"bytes"
	"io"
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
)

type varIntTest struct {
	Name   string
	Value  uint64
	Bytes  []byte
	ExpErr error
}

var writeVarIntTests = []varIntTest{
	{
		Name:  "zero",
		Value: 0x00,
		Bytes: []byte{0x00},
	},
	{
		Name:  "one byte high",
		Value: 0xfc,
		Bytes: []byte{0xfc},
	},
	{
		Name:  "two byte low",
		Value: 0xfd,
		Bytes: []byte{0xfd, 0x00, 0xfd},
	},
	{
		Name:  "two byte high",
		Value: 0xffff,
		Bytes: []byte{0xfd, 0xff, 0xff},
	},
	{
		Name:  "four byte low",
		Value: 0x10000,
		Bytes: []byte{0xfe, 0x00, 0x01, 0x00, 0x00},
	},
	{
		Name:  "four byte high",
		Value: 0xffffffff,
		Bytes: []byte{0xfe, 0xff, 0xff, 0xff, 0xff},
	},
	{
		Name:  "eight byte low",
		Value: 0x100000000,
		Bytes: []byte{0xff, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00},
	},
	{
		Name:  "eight byte high",
		Value: math.MaxUint64,
		Bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	},
}

// TestWriteVarInt asserts the behavior of tlv.WriteVarInt under various
// positive and negative test cases.
func TestWriteVarInt(t *testing.T) {
	for _, test := range writeVarIntTests {
		t.Run(test.Name, func(t *testing.T) {
			testWriteVarInt(t, test)
		})
	}
}

func testWriteVarInt(t *testing.T, test varIntTest) {
	var (
		w   bytes.Buffer
		buf [8]byte
	)
	err := tlv.WriteVarInt(&w, test.Value, &buf)
	if err != nil {
		t.Fatalf("unable to encode %d as varint: %v",
			test.Value, err)
	}

	if !bytes.Equal(w.Bytes(), test.Bytes) {
		t.Fatalf("expected bytes: %v, got %v",
			test.Bytes, w.Bytes())
	}
}

var readVarIntTests = []varIntTest{
	{
		Name:  "zero",
		Value: 0x00,
		Bytes: []byte{0x00},
	},
	{
		Name:  "one byte high",
		Value: 0xfc,
		Bytes: []byte{0xfc},
	},
	{
		Name:  "two byte low",
		Value: 0xfd,
		Bytes: []byte{0xfd, 0x00, 0xfd},
	},
	{
		Name:  "two byte high",
		Value: 0xffff,
		Bytes: []byte{0xfd, 0xff, 0xff},
	},
	{
		Name:  "four byte low",
		Value: 0x10000,
		Bytes: []byte{0xfe, 0x00, 0x01, 0x00, 0x00},
	},
	{
		Name:  "four byte high",
		Value: 0xffffffff,
		Bytes: []byte{0xfe, 0xff, 0xff, 0xff, 0xff},
	},
	{
		Name:  "eight byte low",
		Value: 0x100000000,
		Bytes: []byte{0xff, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00},
	},
	{
		Name:  "eight byte high",
		Value: math.MaxUint64,
		Bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	},
	{
		Name:   "two byte not canonical",
		Bytes:  []byte{0xfd, 0x00, 0xfc},
		ExpErr: tlv.ErrVarIntNotCanonical,
	},
	{
		Name:   "four byte not canonical",
		Bytes:  []byte{0xfe, 0x00, 0x00, 0xff, 0xff},
		ExpErr: tlv.ErrVarIntNotCanonical,
	},
	{
		Name:   "eight byte not canonical",
		Bytes:  []byte{0xff, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff},
		ExpErr: tlv.ErrVarIntNotCanonical,
	},
	{
		Name:   "two byte short read",
		Bytes:  []byte{0xfd, 0x00},
		ExpErr: io.ErrUnexpectedEOF,
	},
	{
		Name:   "four byte short read",
		Bytes:  []byte{0xfe, 0xff, 0xff},
		ExpErr: io.ErrUnexpectedEOF,
	},
	{
		Name:   "eight byte short read",
		Bytes:  []byte{0xff, 0xff, 0xff, 0xff, 0xff},
		ExpErr: io.ErrUnexpectedEOF,
	},
	{
		Name:   "one byte no read",
		Bytes:  []byte{},
		ExpErr: io.EOF,
	},
	// The following cases are the reason for needing to make a custom
	// version of the varint for the tlv package. For the varint encodings
	// in btcd's wire package these would return io.EOF, since it is
	// actually a composite of two calls to io.ReadFull. In TLV, we need to
	// be able to distinguish whether no bytes were read at all from no
	// Bytes being read on the second read as the latter is not a proper TLV
	// stream. We handle this by returning io.ErrUnexpectedEOF if we
	// encounter io.EOF on any of these secondary reads for larger values.
	{
		Name:   "two byte no read",
		Bytes:  []byte{0xfd},
		ExpErr: io.ErrUnexpectedEOF,
	},
	{
		Name:   "four byte no read",
		Bytes:  []byte{0xfe},
		ExpErr: io.ErrUnexpectedEOF,
	},
	{
		Name:   "eight byte no read",
		Bytes:  []byte{0xff},
		ExpErr: io.ErrUnexpectedEOF,
	},
}

// TestReadVarInt asserts the behavior of tlv.ReadVarInt under various positive
// and negative test cases.
func TestReadVarInt(t *testing.T) {
	for _, test := range readVarIntTests {
		t.Run(test.Name, func(t *testing.T) {
			testReadVarInt(t, test)
		})
	}
}

func testReadVarInt(t *testing.T, test varIntTest) {
	var buf [8]byte
	r := bytes.NewReader(test.Bytes)
	val, err := tlv.ReadVarInt(r, &buf)
	if err != nil && err != test.ExpErr {
		t.Fatalf("expected decoding error: %v, got: %v",
			test.ExpErr, err)
	}

	// If we expected a decoding error, there's no point checking the value.
	if test.ExpErr != nil {
		return
	}

	if val != test.Value {
		t.Fatalf("expected value: %d, got %d", test.Value, val)
	}
}
