package tlv_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
)

var tuint16Tests = []struct {
	value uint16
	size  uint64
	bytes []byte
}{
	{
		value: 0x0000,
		size:  0,
		bytes: []byte{},
	},
	{
		value: 0x0001,
		size:  1,
		bytes: []byte{0x01},
	},
	{
		value: 0x00ff,
		size:  1,
		bytes: []byte{0xff},
	},
	{
		value: 0x0100,
		size:  2,
		bytes: []byte{0x01, 0x00},
	},
	{
		value: 0xffff,
		size:  2,
		bytes: []byte{0xff, 0xff},
	},
}

// TestSizeTUint16 asserts that SizeTUint16 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint16(t *testing.T) {
	for _, test := range tuint16Tests {
		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint16(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}

// TestTUint16 asserts that ETUint16 outputs the proper encoding of a truncated
// uint16, and that DTUint16 is able to parse the output.
func TestTUint16(t *testing.T) {
	var buf [8]byte
	for _, test := range tuint16Tests {
		test := test

		if len(test.bytes) != int(test.size) {
			t.Fatalf("invalid test case, "+
				"len(bytes)[%d] != size[%d]",
				len(test.bytes), test.size)
		}

		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			// Test generic encoder.
			var b bytes.Buffer
			err := tlv.ETUint16(&b, &test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint16: %v", err)
			}

			if !bytes.Equal(b.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b.Bytes())
			}

			// Test non-generic encoder.
			var b2 bytes.Buffer
			err = tlv.ETUint16T(&b2, test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint16: %v", err)
			}

			if !bytes.Equal(b2.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b2.Bytes())
			}

			var value uint16
			r := bytes.NewReader(b.Bytes())
			err = tlv.DTUint16(r, &value, &buf, test.size)
			if err != nil {
				t.Fatalf("unable to decode tuint16: %v", err)
			}

			if value != test.value {
				t.Fatalf("decoded value mismatch, "+
					"expected: %d, got: %d",
					test.value, value)
			}
		})
	}
}

var tuint32Tests = []struct {
	value uint32
	size  uint64
	bytes []byte
}{
	{
		value: 0x00000000,
		size:  0,
		bytes: []byte{},
	},
	{
		value: 0x00000001,
		size:  1,
		bytes: []byte{0x01},
	},
	{
		value: 0x000000ff,
		size:  1,
		bytes: []byte{0xff},
	},
	{
		value: 0x00000100,
		size:  2,
		bytes: []byte{0x01, 0x00},
	},
	{
		value: 0x0000ffff,
		size:  2,
		bytes: []byte{0xff, 0xff},
	},
	{
		value: 0x00010000,
		size:  3,
		bytes: []byte{0x01, 0x00, 0x00},
	},
	{
		value: 0x00ffffff,
		size:  3,
		bytes: []byte{0xff, 0xff, 0xff},
	},
	{
		value: 0x01000000,
		size:  4,
		bytes: []byte{0x01, 0x00, 0x00, 0x00},
	},
	{
		value: 0xffffffff,
		size:  4,
		bytes: []byte{0xff, 0xff, 0xff, 0xff},
	},
}

// TestSizeTUint32 asserts that SizeTUint32 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint32(t *testing.T) {
	for _, test := range tuint32Tests {
		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint32(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}

// TestTUint32 asserts that ETUint32 outputs the proper encoding of a truncated
// uint32, and that DTUint32 is able to parse the output.
func TestTUint32(t *testing.T) {
	var buf [8]byte
	for _, test := range tuint32Tests {
		test := test

		if len(test.bytes) != int(test.size) {
			t.Fatalf("invalid test case, "+
				"len(bytes)[%d] != size[%d]",
				len(test.bytes), test.size)
		}

		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			// Test generic encoder.
			var b bytes.Buffer
			err := tlv.ETUint32(&b, &test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint32: %v", err)
			}

			if !bytes.Equal(b.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b.Bytes())
			}

			// Test non-generic encoder.
			var b2 bytes.Buffer
			err = tlv.ETUint32T(&b2, test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint32: %v", err)
			}

			if !bytes.Equal(b2.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b2.Bytes())
			}

			var value uint32
			r := bytes.NewReader(b.Bytes())
			err = tlv.DTUint32(r, &value, &buf, test.size)
			if err != nil {
				t.Fatalf("unable to decode tuint32: %v", err)
			}

			if value != test.value {
				t.Fatalf("decoded value mismatch, "+
					"expected: %d, got: %d",
					test.value, value)
			}
		})
	}
}

var tuint64Tests = []struct {
	value uint64
	size  uint64
	bytes []byte
}{
	{
		value: 0x0000000000000000,
		size:  0,
		bytes: []byte{},
	},
	{
		value: 0x0000000000000001,
		size:  1,
		bytes: []byte{0x01},
	},
	{
		value: 0x00000000000000ff,
		size:  1,
		bytes: []byte{0xff},
	},
	{
		value: 0x0000000000000100,
		size:  2,
		bytes: []byte{0x01, 0x00},
	},
	{
		value: 0x000000000000ffff,
		size:  2,
		bytes: []byte{0xff, 0xff},
	},
	{
		value: 0x0000000000010000,
		size:  3,
		bytes: []byte{0x01, 0x00, 0x00},
	},
	{
		value: 0x0000000000ffffff,
		size:  3,
		bytes: []byte{0xff, 0xff, 0xff},
	},
	{
		value: 0x0000000001000000,
		size:  4,
		bytes: []byte{0x01, 0x00, 0x00, 0x00},
	},
	{
		value: 0x00000000ffffffff,
		size:  4,
		bytes: []byte{0xff, 0xff, 0xff, 0xff},
	},
	{
		value: 0x0000000100000000,
		size:  5,
		bytes: []byte{0x01, 0x00, 0x00, 0x00, 0x00},
	},
	{
		value: 0x000000ffffffffff,
		size:  5,
		bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff},
	},
	{
		value: 0x0000010000000000,
		size:  6,
		bytes: []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00},
	},
	{
		value: 0x0000ffffffffffff,
		size:  6,
		bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	},
	{
		value: 0x0001000000000000,
		size:  7,
		bytes: []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	},
	{
		value: 0x00ffffffffffffff,
		size:  7,
		bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	},
	{
		value: 0x0100000000000000,
		size:  8,
		bytes: []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	},
	{
		value: 0xffffffffffffffff,
		size:  8,
		bytes: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	},
}

// TestSizeTUint64 asserts that SizeTUint64 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint64(t *testing.T) {
	for _, test := range tuint64Tests {
		if len(test.bytes) != int(test.size) {
			t.Fatalf("invalid test case, "+
				"len(bytes)[%d] != size[%d]",
				len(test.bytes), test.size)
		}

		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint64(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}

// TestTUint64 asserts that ETUint64 outputs the proper encoding of a truncated
// uint64, and that DTUint64 is able to parse the output.
func TestTUint64(t *testing.T) {
	var buf [8]byte
	for _, test := range tuint64Tests {
		test := test

		if len(test.bytes) != int(test.size) {
			t.Fatalf("invalid test case, "+
				"len(bytes)[%d] != size[%d]",
				len(test.bytes), test.size)
		}

		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			// Test generic encoder.
			var b bytes.Buffer
			err := tlv.ETUint64(&b, &test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint64: %v", err)
			}

			if !bytes.Equal(b.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b.Bytes())
			}

			// Test non-generic encoder.
			var b2 bytes.Buffer
			err = tlv.ETUint64T(&b2, test.value, &buf)
			if err != nil {
				t.Fatalf("unable to encode tuint64: %v", err)
			}

			if !bytes.Equal(b2.Bytes(), test.bytes) {
				t.Fatalf("encoding mismatch, "+
					"expected: %x, got: %x",
					test.bytes, b2.Bytes())
			}

			var value uint64
			r := bytes.NewReader(b.Bytes())
			err = tlv.DTUint64(r, &value, &buf, test.size)
			if err != nil {
				t.Fatalf("unable to decode tuint64: %v", err)
			}

			if value != test.value {
				t.Fatalf("decoded value mismatch, "+
					"expected: %d, got: %d",
					test.value, value)
			}
		})
	}
}
